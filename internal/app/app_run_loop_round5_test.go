// Round-5 fixes of the `rush run` loop (docs/reviews/2026-09-30-async-phase4-
// round5.md, W5-APP: R5C-1, R5C-2, R5C-3): same harness as the round-4 tests.
package app

import (
	"bytes"
	"context"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// fillCallOptions sets every field of agent.CallOptions to a non-zero value.
func fillCallOptions(t *testing.T) *agent.CallOptions {
	t.Helper()
	opts := &agent.CallOptions{}
	v := reflect.ValueOf(opts).Elem()
	for i := range v.NumField() {
		f, name := v.Field(i), v.Type().Field(i).Name
		switch f.Kind() {
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int, reflect.Int64:
			f.SetInt(7)
		case reflect.Float64:
			f.SetFloat(1.5)
		case reflect.String:
			f.SetString("x")
		case reflect.Pointer:
			f.Set(reflect.New(f.Type().Elem()))
		case reflect.Interface:
			switch f.Type() {
			case reflect.TypeFor[tools.DiskProvider]():
				f.Set(reflect.ValueOf(tools.OSDisk()))
			default:
				t.Fatalf("CallOptions.%s: teach fillCallOptions its type %s", name, f.Type())
			}
		default:
			t.Fatalf("CallOptions.%s: teach fillCallOptions kind %s", name, f.Kind())
		}
	}
	return opts
}

// R5C-1: the reviewer's CallOptions carry every field of the caller's, except
// the two overridden on purpose. Before the fix SupervisionDisabled and
// SupervisionInterval were dropped, so the reviewer's jobs (waited on since
// R4C-1) armed supervision the operator had turned off.
//
// Revert-check: dropping the two Supervision* copies fails on those fields.
func TestReviewerCallOptionsCarryEveryPrimaryField(t *testing.T) {
	overridden := map[string]bool{"ModelRole": true, "DisableSubAgents": true}
	primary := fillCallOptions(t)
	// Distinct from the values the reviewer forces, so a copy is visible.
	primary.ModelRole = config.SelectedModelTypeSmart
	primary.DisableSubAgents = false
	primary.FolderScope = &permission.FolderScope{}

	got := reviewerCallOptions(primary)

	pv, gv := reflect.ValueOf(*primary), reflect.ValueOf(*got)
	for i := range pv.NumField() {
		name := pv.Type().Field(i).Name
		if overridden[name] {
			require.NotEqual(t, pv.Field(i).Interface(), gv.Field(i).Interface(), "%s is overridden on purpose", name)
			continue
		}
		require.Equal(t, pv.Field(i).Interface(), gv.Field(i).Interface(), "CallOptions.%s must carry over to the reviewer (or join the overridden list on purpose)", name)
	}
	require.Equal(t, config.SelectedModelTypeReviewer, got.ModelRole)
	require.True(t, got.DisableSubAgents)
}

// supervisionNotices counts the session's supervision check-in rows.
func supervisionNotices(t *testing.T, rh *reviewerAsyncHarness) int {
	t.Helper()
	notices, err := rh.app.asyncJobStore.ListSessionNotices(context.Background(), rh.sessionID)
	require.NoError(t, err)
	n := 0
	for _, no := range notices {
		if no.Kind == session.NoticeKindSupervision {
			n++
		}
	}
	return n
}

// waitingOnWrite signals once the loop prints its "still has open work" line.
type waitingOnWrite struct {
	syncBuffer
	waiting chan struct{}
	fired   atomic.Bool
}

func (w *waitingOnWrite) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if strings.Contains(string(p), "still has open work") && w.fired.CompareAndSwap(false, true) {
		close(w.waiting)
	}
	return n, err
}

// runHeldReviewerJob runs the loop with a reviewer whose bash job never
// finishes, waits until the loop waits on it, then calls observe, then cancels
// (Ctrl-C) and returns the run's error.
func runHeldReviewerJob(t *testing.T, rh *reviewerAsyncHarness, overrides RunOverrides, observe func()) error {
	t.Helper()
	ctx, cancel := context.WithCancel(loopCtx(t))
	defer cancel()
	overrides.Origin = message.OriginCLI
	turnOverrides := overrides
	stderr := &waitingOnWrite{waiting: make(chan struct{})}
	var out syncBuffer
	l := &cliLoop{
		app: rh.app, source: driverSource(t, rh.app), ctx: ctx, output: &out, mode: RunModeJSON, hideSpinner: true,
		overrides: overrides, turnOverrides: turnOverrides, prompt: "do it",
		continueSessionID: rh.sessionID, started: time.Now(), sessionID: rh.sessionID,
		lastBuffered: &bytes.Buffer{}, stderr: stderr,
	}
	done := make(chan error, 1)
	go func() {
		_, err := l.run()
		done <- err
	}()
	select {
	case <-stderr.waiting:
	case err := <-done:
		t.Fatalf("the loop ended before it waited on the reviewer's job: %v", err)
	case <-ctx.Done():
		t.Fatal("the loop never waited on the reviewer's job")
	}
	observe()
	cancel()
	return <-done
}

// R5C-1: `--no-supervision` covers the reviewer's jobs too. The reviewer's
// bash job is held open past several (shrunk) supervision intervals; no
// check-in notice may be written and no Drain may start. Control: without the
// flag the same run does get a check-in, so the shrunk interval is really
// armed.
//
// Revert-check: dropping the SupervisionDisabled copy in reviewerCallOptions
// makes the no-supervision case write a check-in after ~1s.
func TestRunLoop_ReviewerJobHonorsNoSupervision(t *testing.T) {
	t.Cleanup(agent.SetSupervisionDefaultIntervalForTest(1100 * time.Millisecond))
	for _, tc := range []struct {
		name          string
		noSupervision bool
	}{{"disabled", true}, {"control_enabled", false}} {
		t.Run(tc.name, func(t *testing.T) {
			rh := newReviewerAsyncHarness(t, true)
			err := runHeldReviewerJob(t, rh, RunOverrides{ModelRole: config.SelectedModelTypeSmart, NoSupervision: tc.noSupervision}, func() {
				if !tc.noSupervision {
					require.Eventually(t, func() bool { return supervisionNotices(t, rh) > 0 }, 20*time.Second, 50*time.Millisecond,
						"control: the shrunk supervision interval must fire for the reviewer's job")
					return
				}
				time.Sleep(3300 * time.Millisecond) // three intervals
				require.Zero(t, supervisionNotices(t, rh), "--no-supervision: the reviewer's job arms no check-in")
				require.EqualValues(t, 3, rh.requests.Load(), "no paid Drain on a supervision notice: first turn + the reviewer's two steps")
			})
			require.ErrorIs(t, err, context.Canceled)
		})
	}
}

// R5C-1: `--supervision-interval` applies to the reviewer's jobs too (default
// left at 5 minutes: only the carried interval can fire within the test).
//
// Revert-check: dropping the SupervisionInterval copy leaves the reviewer on
// the 5-minute default and no check-in ever arrives.
func TestRunLoop_ReviewerJobHonorsSupervisionInterval(t *testing.T) {
	rh := newReviewerAsyncHarness(t, true)
	err := runHeldReviewerJob(t, rh, RunOverrides{ModelRole: config.SelectedModelTypeSmart, SupervisionInterval: 1200 * time.Millisecond}, func() {
		require.Eventually(t, func() bool { return supervisionNotices(t, rh) > 0 }, 20*time.Second, 50*time.Millisecond,
			"the operator's --supervision-interval must reach the reviewer's job")
	})
	require.ErrorIs(t, err, context.Canceled)
}

// refusalRetryWriter runs onRefused (once) when the loop reports a refused
// Drain and counts those lines.
type refusalRetryWriter struct {
	syncBuffer
	onRefused func()
	refused   atomic.Int32
	gaveUp    atomic.Bool
}

func (w *refusalRetryWriter) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	s := string(p)
	if strings.Contains(s, "giving up") {
		w.gaveUp.Store(true)
	}
	if strings.Contains(s, "was refused") && w.refused.Add(1) == 1 && w.onRefused != nil {
		w.onRefused()
	}
	return n, err
}

// R5C-2: a refusal streak that ended with the scope closing (its debt was
// settled by another owner) must not count against the reviewer's Drains: the
// first Drain refusal after the reviewer gets a fresh retry budget, its own
// "retrying" line, and the run ends with the reaction. Before the fix the old
// refusalSince made that refusal exceed the 30s budget at once ("giving up",
// exit_reason error, the job result never reacted to).
//
// Revert-check: dropping the reset at stepExit in cliLoop.run makes the run
// end with the refusal error and no reaction.
func TestCLILoop_RefusalStreakDoesNotOutliveClosedScope(t *testing.T) {
	defer agent.SetDrainPacingForTest(0, 100*time.Millisecond, 0)()
	rh := newReviewerAsyncHarness(t, false)
	setPeak := func(on bool) {
		cfg, ok := rh.app.config.Config().Providers.Get("openaicompat")
		require.True(t, ok)
		cfg.PeakHours = nil
		if on {
			cfg.PeakHours = &config.PeakHoursWindow{Start: "00:00", End: "23:59"}
		}
		rh.app.config.Config().Providers.Set("openaicompat", cfg)
	}
	stderr := &refusalRetryWriter{onRefused: func() { setPeak(false) }}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(stderr.String())
		}
	})
	var turns atomic.Int32
	cliLoopTurnDoneSeam = func() {
		// The reviewer turn is the loop's second turn: from here on Drains are
		// refused until the first refusal line is printed.
		if turns.Add(1) == 2 {
			setPeak(true)
		}
	}
	t.Cleanup(func() { cliLoopTurnDoneSeam = nil })
	var out syncBuffer
	l := r4LoopWith(loopCtx(t), rh, driverSource(t, rh.app), &out, stderr)
	l.streak = drainStreak{since: time.Now().Add(-time.Minute)} // a streak from before the scope closed

	res, err := l.run()

	require.NoError(t, err)
	require.False(t, stderr.gaveUp.Load(), "the stale streak must not exhaust the new budget")
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, r4Verdict, res.FinalText, "the answer is the reaction the refused Drain finally ran")
	require.EqualValues(t, 1, stderr.refused.Load(), "one refusal, one 'retrying' line")
	starts, drains := rh.snapshot()
	require.Equal(t, 1, starts)
	require.Len(t, drains, 1)
	rh.requireNothingOpen(t)
}

const (
	r5ChildFirstCost = 1.0 // 100000 prompt tokens at $10 per million
	r5ChildLaterCost = 0.5 // 50000 prompt tokens
)

func loopToolCallWithUsage(w http.ResponseWriter, id, callID, name, args string, prompt int) {
	admissionWriteSSE(w, []string{
		admissionSSEToolCall(id, callID, name, args),
		`{"id":"` + id + `","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":` +
			strconv.Itoa(prompt) + `,"completion_tokens":0,"total_tokens":` + strconv.Itoa(prompt) + `}}`,
	})
}

// newRunningChildHarness scripts a root that delegates to an async child: the
// child's first provider call (a tool call) is charged r5ChildFirstCost, its
// second request blocks (childBlocked closes when it arrives) until release
// closes, then answers at r5ChildLaterCost.
func newRunningChildHarness(t *testing.T) (h *loopHarness, childBlocked <-chan struct{}, release func()) {
	t.Helper()
	blocked, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h = newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		system, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(system, "short title"):
			loopText(w, "title", "Title", 5, 1)
		case strings.Contains(lastUser, "WORKER-MARKER") && lastTool != "":
			once.Do(func() { close(blocked) })
			select {
			case <-gate:
			case <-h.t.Context().Done():
				return
			}
			loopText(w, "child-done", "child done", 50000, 0)
		case strings.Contains(lastUser, "WORKER-MARKER"):
			loopToolCallWithUsage(w, "child-ls", "call-ls", "ls", `{"path":"."}`, 100000)
		case strings.Contains(lastTool, "Async agent job call-agent started"):
			loopText(w, "root-yield", "root waiting", 11, 3)
		case strings.Contains(lastUser, "Async job call-agent (agent)"):
			loopText(w, "root-final", "R5-ROOT-DONE", 11, 3)
		default:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER build it"}`),
				admissionSSEStop("delegate", "tool_calls"),
			})
		}
	})
	r4Models(t, h.app, 10)
	return h, blocked, func() { close(gate) }
}

// childSessionOf returns the async delegation child of the harness's root.
func (h *loopHarness) childSessionOf(t *testing.T) string {
	t.Helper()
	rows, err := h.app.asyncJobStore.ListAsyncJobsForOwner(context.Background(), h.sessionID)
	require.NoError(t, err)
	for _, row := range rows {
		if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
			return row.ChildSessionID.String
		}
	}
	t.Fatal("no delegation row")
	return ""
}

func (h *loopHarness) costOf(t *testing.T, id string) float64 {
	t.Helper()
	// #1130: the root's "cost" is the subtree spend; the root's own row
	// never carries the child's.
	spent, err := h.app.Sessions.SubtreeSpent(context.Background(), id)
	require.NoError(t, err)
	return spent
}

// R5C-3, subtree-read model (#1130): a cancel exit (Ctrl-C, --timeout) with a
// delegation still running reports the child's spend so far in the envelope:
// the window is a subtree difference, and the running child charges its own
// cost_self the moment it spends, so the exit reading already contains it.
// The later spend -- the child finishing, or App.Shutdown cancelling it --
// grows the child's own ledger and, with it, every later budget read, never
// the envelope that already flushed.
//
// Revert-check: windowing over the root row's own ledger again (or summing
// the turns' deltas) drops the running child: the envelope reports a
// fraction of a cent next to an error naming a dollar.
func TestRunNonInteractive_CancelExitCostIncludesRunningChildSpend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		childLate float64 // what the child spends after the envelope (0: Shutdown cancels it)
	}{
		{"shutdown_cancels_child", 0},
		{"child_finishes_after_exit", r5ChildLaterCost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, childBlocked, release := newRunningChildHarness(t)
			ctx, cancel := context.WithCancel(loopCtx(t))
			defer cancel()
			type outcome struct {
				res *RunResult
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				res, _, err := h.run(ctx, RunOverrides{})
				done <- outcome{res, err}
			}()
			select {
			case <-childBlocked:
			case <-time.After(30 * time.Second):
				t.Fatal("the child never reached its second request")
			}
			// At this point the subtree already holds the running child's
			// first-turn spend: the child paid its own row the moment it spent.
			spentBeforeExit := h.sessionCost(t)
			require.GreaterOrEqual(t, spentBeforeExit, r5ChildFirstCost,
				"the running child's spend is in the subtree before any exit")
			cancel()
			var got outcome
			select {
			case got = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("the loop did not exit on cancel")
			}

			require.ErrorIs(t, got.err, context.Canceled)
			require.NotNil(t, got.res)
			require.Equal(t, "canceled", got.res.ExitReason)
			require.GreaterOrEqual(t, got.res.Usage.DeltaCostUSD, r5ChildFirstCost,
				"the envelope counts the running child's spend so far")
			require.GreaterOrEqual(t, got.res.Usage.DeltaCostUSD, r5ChildFirstCost,
				"the envelope counts the running child's spend so far")
			require.InDelta(t, spentBeforeExit, got.res.Usage.DeltaCostUSD, 0.001,
				"the envelope reports the subtree spend from the claim to the exit")

			child := h.childSessionOf(t)
			require.InDelta(t, r5ChildFirstCost, h.costOf(t, child), 0.01)
			if tc.childLate > 0 {
				release()
			} else {
				h.app.CancelAgents()
			}
			// The child keeps charging its own ledger after the envelope has
			// flushed; the flushed envelope is untouched, later budget reads
			// see the growth, and nothing is counted twice.
			var childTotal float64
			require.Eventually(t, func() bool {
				childTotal = h.costOf(t, child)
				return childTotal >= r5ChildFirstCost+tc.childLate-0.005
			}, 30*time.Second, 20*time.Millisecond, "the child's own ledger holds its total")

			rootOwn, err := h.app.Sessions.Get(context.Background(), h.sessionID)
			require.NoError(t, err)
			require.InDelta(t, rootOwn.OwnCost+childTotal, h.sessionCost(t), 0.01,
				"the child's spend reaches the root's budget exactly once: the later spend is only the delta")
		})
	}
}

// R5C-3, rewritten for the subtree-read model (#1130): nested delegations
// sum bottom-up in one query (grandchild into child's subtree, child into
// root's), and repeating the read moves nothing — there is no transfer left
// to double-fire.
//
// Revert-check: a tree walk that is not a true recursion (e.g. a fixed-depth
// join) under-reports the root; a UNION ALL CTE double-counts a node reachable
// by two paths.
func TestChargeRunningChildren_NestedDelegationsBottomUpAndIdempotent(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "first answer", 11, 3)
	})
	ctx := context.Background()
	spend := func(id string, cost float64) {
		_, err := h.app.Sessions.IncrementCost(ctx, id, cost)
		require.NoError(t, err)
	}
	root, err := h.app.Sessions.Create(ctx, "root")
	require.NoError(t, err)
	spend(root.ID, 0.1)
	child, err := h.app.Sessions.CreateTaskSession(ctx, "deleg-child", root.ID, "child")
	require.NoError(t, err)
	spend(child.ID, 0.2)
	grandchild, err := h.app.Sessions.CreateTaskSession(ctx, "deleg-grandchild", child.ID, "grandchild")
	require.NoError(t, err)
	spend(grandchild.ID, 0.3)

	require.InDelta(t, 0.6, h.costOf(t, root.ID), 1e-9, "root: own 0.1 + child 0.2 + grandchild 0.3")
	require.InDelta(t, 0.5, h.costOf(t, child.ID), 1e-9, "child: own 0.2 + grandchild 0.3")
	require.InDelta(t, 0.3, h.costOf(t, grandchild.ID), 1e-9)

	require.InDelta(t, 0.6, h.costOf(t, root.ID), 1e-9, "a repeated read moves nothing")
	require.InDelta(t, 0.5, h.costOf(t, child.ID), 1e-9)
}
