// Round-5 fixes of the `rush run` loop (docs/reviews/2026-09-30-async-phase4-
// round5.md, W5-APP: R5C-1, R5C-2, R5C-3): same harness as the round-4 tests.
package app

import (
	"bytes"
	"context"
	"reflect"
	"strings"
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
		case reflect.Ptr:
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
	l.refusalSince = time.Now().Add(-time.Minute) // a streak from before the scope closed

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
