// Round-3 fixes of the `rush run` loop (docs/reviews/2026-09-30-async-phase4-
// round3.md, W3-APP: R3C-1, -3, -4, -6, -7, -8): same harness as
// app_run_loop_test.go (real App and SQLite, httptest provider).
package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	r3FirstAnswer   = "R3-FIRST-ANSWER"
	r3ReactionText  = "R3-REACTION-ANSWER"
	r3ReviewVerdict = "R3-REVIEWER-VERDICT"
)

// reviewerHarness answers the first turn, the Drain (a request carrying the
// seeded notice) and the reviewer pass (its fixed prompt) with distinct texts
// and usage, and records the reviewer's requests.
type reviewerHarness struct {
	*loopHarness
	mu       sync.Mutex
	reviewer []string // bodies of the reviewer requests
}

func newReviewerHarness(t *testing.T, seedDuringFirstTurn bool) *reviewerHarness {
	t.Helper()
	rh := &reviewerHarness{}
	rh.loopHarness = newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, body []byte, drain bool, n int) {
		_, lastUser, _ := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "independent reviewer"):
			rh.mu.Lock()
			rh.reviewer = append(rh.reviewer, string(body))
			rh.mu.Unlock()
			loopText(w, "r", r3ReviewVerdict, 60, 5)
		case drain:
			loopText(w, "d", r3ReactionText, 30, 10)
		default:
			if seedDuringFirstTurn && n == 1 {
				// Seeded while the turn runs, after its own pull: the debt is owed
				// when the turn ends (an async job that finished mid-turn).
				h.seedDebt()
			}
			loopText(w, "a", r3FirstAnswer, 11, 3)
		}
	})
	rh.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "probe"})
	return rh
}

func (rh *reviewerHarness) reviewerBodies() []string {
	rh.mu.Lock()
	defer rh.mu.Unlock()
	return append([]string(nil), rh.reviewer...)
}

// R3C-1: a smart run with a reviewer whose first turn leaves work owed: the
// Drain reacts, then the reviewer runs ONCE, at the scope-closed exit, on the
// final text (the reaction); A10: the verdict lands in the additive review
// field while final_text stays the executor's answer, with the
// whole run's usage. Before the fix the last Drain ran the reviewer inside its
// own Drain context: it found nothing to pull, came back as a "queued" no-turn,
// the loop dropped it and exited with the FIRST turn's text and usage, the
// reviewer never having seen the reaction.
//
// Revert-check: restoring the old flow (the Drain's ExecuteRun running the
// reviewer, the loop not running it at its exit) leaves the reviewer with no
// request that contains the reaction and the verdict is not the answer.
func TestRunNonInteractive_ReviewerRunsOnceAfterDrainOnFinalText(t *testing.T) {
	rh := newReviewerHarness(t, true)

	res, out, err := rh.run(loopCtx(t), RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	require.NoError(t, err)
	require.NotNil(t, res)
	bodies := rh.reviewerBodies()
	require.Len(t, bodies, 1, "the reviewer pass runs exactly once")
	require.Contains(t, bodies[0], r3ReactionText, "the reviewer reviews the FINAL text, reaction included")
	require.Equal(t, r3ReactionText, res.FinalText, "A10: final_text stays the executor's answer")
	require.Equal(t, r3ReviewVerdict, res.Review, "the verdict is the additive review field")
	require.Contains(t, out, r3ReviewVerdict)
	require.Contains(t, out, r3ReactionText, "the executor's answer is in the envelope")
	require.Equal(t, "end_turn", res.ExitReason)
	require.EqualValues(t, 65, res.Usage.DeltaTokens, "the totals reach the reviewer's turn (session counters are last-snapshot: 14, 40, 65; a dropped reviewer would leave 40)")
	require.False(t, rh.debtOpen())
	require.EqualValues(t, 3, rh.requests.Load(), "first turn, one Drain, one reviewer turn")
}

// R3C-1 (guard): a Drain iteration never runs the reviewer itself, even when
// its caller does not defer the pass to a loop. Driven through ExecuteRun.
//
// Revert-check: dropping `!req.drainTurn` from reviewerCandidate runs the
// review turn inside the Drain (reviewer request, "queued" result).
func TestExecuteRun_DrainTurnNeverRunsTheReviewerPass(t *testing.T) {
	rh := newReviewerHarness(t, false)
	ctx := loopCtx(t)
	rh.seedDebt()

	var out syncBuffer
	res, err := rh.app.ExecuteRun(agent.WithDrainCall(agent.WithBackgroundJobNotice(ctx)), RunRequest{
		Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeSmart, Origin: message.OriginCLI},
		Mode:              RunModeJSON,
		ContinueSessionID: rh.sessionID,
		Origin:            message.OriginCLI,
		Stdout:            &out,
		captureResult:     true,
		drainTurn:         true,
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Empty(t, rh.reviewerBodies(), "a Drain never runs the reviewer pass")
	require.Equal(t, r3ReactionText, res.FinalText)
	require.NotEqual(t, "queued", res.ExitReason)
}

// R3C-3: a setup step that fails after the driver claim but before the user's
// turn is launched (here the reasoning-effort write) ends the run with that
// error. Before the fix the loop carried on: a Drain reacted to the unrelated
// notice and cleared the error, so the run exited 0 although "do it" never
// reached the model.
//
// Revert-check: stopping only when the driver claim failed (the old test in
// run()) lets the Drain reach the provider and exit 0.
func TestRunNonInteractive_FirstTurnFailsBeforeSubmission_EndsTheRun(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Error("nothing may reach the provider: the user's turn never ran")
		loopText(w, "x", "unexpected", 1, 1)
	})
	_, err := h.app.DB().ExecContext(context.Background(), `CREATE TRIGGER fx_block_effort BEFORE UPDATE OF smart_model_reasoning_effort ON sessions
		BEGIN SELECT RAISE(ABORT, 'fx: effort write blocked'); END`)
	require.NoError(t, err)
	h.seedDebt() // an unrelated notice a Drain would happily react to

	res, _, runErr := h.run(loopCtx(t), RunOverrides{ReasoningEffort: "low", RoleSmart: true})

	require.Error(t, runErr)
	require.Contains(t, runErr.Error(), "failed to set reasoning effort")
	require.Nil(t, res, "no turn ran, so there is no envelope")
	require.Zero(t, h.requests.Load())
	require.True(t, h.debtOpen(), "the notice is untouched: no Drain ran")
}

// R3C-4: a Drain that committed right before Ctrl-C/--timeout keeps its answer
// as the run's final text; the run still exits canceled.
//
// Revert-check: leaving `final` on the previous turn shows the first answer.
func TestRunNonInteractive_DrainCommittedBeforeCancel_KeepsItsAnswer(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			loopText(w, "d", r3ReactionText, 30, 10)
			return
		}
		loopText(w, "a", r3FirstAnswer, 11, 3)
	})
	ctx, cancel := context.WithCancel(loopCtx(t))
	var turns atomic.Int32
	cliLoopTurnDoneSeam = func() {
		switch turns.Add(1) {
		case 1:
			h.seedDebt()
		case 2:
			cancel() // lands right after the Drain's ExecuteRun returned
		}
	}
	t.Cleanup(func() { cliLoopTurnDoneSeam = nil })

	res, _, err := h.run(ctx, RunOverrides{})

	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Equal(t, r3ReactionText, res.FinalText, "the committed reaction is the answer")
	require.EqualValues(t, 40, res.Usage.DeltaTokens, "usage of both turns")
}

// R3C-6 (B10): a Drain iteration writes none of the invocation's session
// setup. Between the turns a "web UI" edits the prompt, reasoning effort,
// budget, ended_reason and model slot; the Drain's own request must still see
// the edited values (the setup writes all happen before the provider call) and
// the model edit must survive the Drain. Each assertion pins one guard.
//
// Revert-check: removing any one of the `!req.mutationFree()` guards in
// ExecuteRun (system prompt, reasoning effort, SetBudget, SetEndedReason("")) or
// the one around WithSessionModelPersistence in prepareExecuteRun turns its own
// assertion red.
func TestRunNonInteractive_DrainTurnsMutationFree(t *testing.T) {
	// Get does not read the budget/ended_reason columns: the snapshot reads them
	// straight from the row.
	type rowSnap struct {
		session.Session
		endedReason           sql.NullString
		maxCost               float64
		maxTokens, timeoutSec int64
	}
	var mu sync.Mutex
	var snap rowSnap
	snapped := false
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			sess, err := h.app.Sessions.Get(context.Background(), h.sessionID)
			cur := rowSnap{Session: sess}
			rerr := h.app.DB().QueryRowContext(context.Background(),
				`SELECT ended_reason, COALESCE(budget_max_cost, 0), COALESCE(budget_max_tokens, 0), COALESCE(budget_timeout_sec, 0) FROM sessions WHERE id = ?`,
				h.sessionID).Scan(&cur.endedReason, &cur.maxCost, &cur.maxTokens, &cur.timeoutSec)
			if err == nil && rerr == nil {
				mu.Lock()
				snap, snapped = cur, true
				mu.Unlock()
			}
			loopText(w, "d", r3ReactionText, 30, 10)
			return
		}
		loopText(w, "a", r3FirstAnswer, 11, 3)
	})
	cfg, ok := h.app.config.Config().Providers.Get("openaicompat")
	require.True(t, ok)
	cfg.Models = append(cfg.Models, catwalk.Model{ID: "web-model", Name: "web-model", ContextWindow: 200000, DefaultMaxTokens: 1000})
	h.app.config.Config().Providers.Set("openaicompat", cfg)
	h.afterFirstTurn(func() {
		ctx := context.Background()
		h.seedDebt()
		require.NoError(t, h.app.Sessions.UpdateSystemPrompt(ctx, h.sessionID, "web prompt"))
		require.NoError(t, h.app.Sessions.UpdateReasoningEffort(ctx, h.sessionID, "high", ""))
		require.NoError(t, h.app.Sessions.SetBudget(ctx, h.sessionID, 123, 456, 789))
		require.NoError(t, h.app.Sessions.SetEndedReason(ctx, h.sessionID, "web-ended"))
		require.NoError(t, h.app.Sessions.UpdateModels(ctx, h.sessionID, &session.ModelSlotUpdate{Provider: "openaicompat", Model: "web-model"}, nil))
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{
		SystemPrompt: "run prompt", ReasoningEffort: "low", RoleSmart: true,
		MaxCost: 100, MaxTokens: 1_000_000, Timeout: 10 * time.Minute,
		SmartModel: "openaicompat/probe",
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 2, h.requests.Load(), "the first turn and a real Drain")
	require.Equal(t, r3ReactionText, res.FinalText)
	mu.Lock()
	defer mu.Unlock()
	require.True(t, snapped, "the Drain reached the provider")
	assert.Equal(t, "web prompt", snap.SystemPrompt, "a Drain must not rewrite the system prompt (UpdateSystemPrompt guard)")
	assert.Equal(t, "high", snap.SmartModelReasoningEffort, "a Drain must not rewrite the reasoning effort (UpdateReasoningEffort guard)")
	assert.EqualValues(t, 123, snap.maxCost, "a Drain must not rewrite the budget (SetBudget guard)")
	assert.EqualValues(t, 456, snap.maxTokens)
	assert.EqualValues(t, 789, snap.timeoutSec)
	assert.Equal(t, "web-ended", snap.endedReason.String, "a Drain must not clear ended_reason at setup (SetEndedReason guard)")
	final, gerr := h.app.Sessions.Get(context.Background(), h.sessionID)
	require.NoError(t, gerr)
	assert.Equal(t, "web-model", final.SmartModelID, "a Drain must not re-persist the --model override (model persistence guard)")
}

// R3C-6 (cancel flag): a Drain iteration does not clear a cancel request that
// lands after the pre-launch check (`sessions cancel` between the check and
// the Drain's setup): the flag is the operator's, and the Drain's own
// step-boundary check honours it.
//
// Revert-check: dropping the guard around ClearCancelRequest clears the flag
// and this test goes red.
func TestRunNonInteractive_DrainKeepsCancelRequest(t *testing.T) {
	defer agent.SetDrainPacingForTest(50*time.Millisecond, 0, 0)()
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			loopText(w, "d", r3ReactionText, 30, 10)
			return
		}
		loopText(w, "a", r3FirstAnswer, 11, 3)
	})
	h.afterFirstTurn(h.seedDebt)
	var once sync.Once
	cliLoopBeforeDrainSeam = func() {
		once.Do(func() {
			require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
		})
	}
	t.Cleanup(func() { cliLoopBeforeDrainSeam = nil })

	_, _, _ = h.run(loopCtx(t), RunOverrides{})

	require.EqualValues(t, 1, h.drains.Load(), "the Drain ran (this is not the pre-launch cancel check)")
	canceled, err := h.app.Sessions.IsCancelRequested(context.Background(), h.sessionID)
	require.NoError(t, err)
	require.True(t, canceled, "a Drain iteration must not clear the operator's cancel request")
}

// B10 proper: an operator's `sessions cancel` between turns ends the run
// (canceled) before any paid reaction turn.
//
// Revert-check: dropping the pre-launch cancel check launches the Drain (a
// second request) and this test goes red.
func TestRunNonInteractive_CancelRequestBeforeDrainEndsRun(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			t.Error("no Drain may be launched after `sessions cancel`")
		}
		loopText(w, "a", r3FirstAnswer, 11, 3)
	})
	h.afterFirstTurn(func() {
		h.seedDebt()
		require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
	})

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.Error(t, err)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.EqualValues(t, 1, h.requests.Load())
}

// R3C-8: every failed paced attempt prints one stderr line naming the error
// and when the retry comes; waking early from the wait never repeats it.
//
// Revert-check: dropping the noticePaced call leaves stderr silent.
func TestRunNonInteractive_FailedDrainPrintsOneLinePerAttempt(t *testing.T) {
	defer agent.SetDrainPacingForTest(200*time.Millisecond, 0, 0)()
	var stderr syncBuffer
	cliLoopStderr = &stderr
	t.Cleanup(func() { cliLoopStderr = nil })
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"message":"forbidden by the fronting balancer","type":"x"}}`))
			return
		}
		loopText(w, "a", r3FirstAnswer, 11, 3)
	})
	h.afterFirstTurn(h.seedDebt)

	_, _, err := h.run(loopCtx(t), RunOverrides{})

	require.Error(t, err)
	require.EqualValues(t, 3, h.drains.Load(), "three counted attempts")
	var lines []string
	for _, line := range strings.Split(stderr.String(), "\n") {
		if strings.Contains(line, "the reaction turn failed") {
			lines = append(lines, line)
		}
	}
	require.Len(t, lines, 2, "one line per failed attempt that is retried (the third closes the debt)")
	for _, line := range lines {
		require.Contains(t, line, h.sessionID)
		require.Contains(t, line, "retrying at ")
	}
}

// R3C-1 audit: an iteration that queued behind another owner runs no turn of
// the loop's and is not accounted as one, but what the session spends meanwhile
// is the run's cost: it is folded in when the next turn starts or the run ends.
//
// Revert-check: dropping queuedMark (the old branch that returned before any
// accounting) leaves the owner's spend out of the totals.
func TestCLILoop_QueuedIterationUsageIsFoldedIntoTheRun(t *testing.T) {
	h := newLoopHarness(t, func(h *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", r3FirstAnswer, 11, 3)
	})
	ctx := loopCtx(t)
	l := &cliLoop{
		app: h.app, source: &scriptedScopeSource{script: []scopeAnswer{{}}}, ctx: ctx,
		output: io.Discard, mode: RunModeJSON, started: time.Now(),
		sessionID: h.sessionID, lastBuffered: &bytes.Buffer{},
		final: &RunResult{SessionID: h.sessionID, ExitReason: "stop", FinalText: r3FirstAnswer},
	}
	before := l.sessionUsage()

	done, err := l.afterDrain(&RunResult{ExitReason: "queued"}, &runQueuedError{sessionID: h.sessionID}, &bytes.Buffer{}, before)
	require.False(t, done)
	require.NoError(t, err)
	require.NoError(t, h.app.Sessions.SetUsage(ctx, h.sessionID, 100, 50)) // the owner's turn spends

	res, exitErr := l.exit(nil, "")

	require.NoError(t, exitErr)
	require.EqualValues(t, 150, res.Usage.DeltaTokens, "the owner's spend is in the run's totals")
	require.Equal(t, r3FirstAnswer, res.FinalText, "a queued iteration never replaces the answer")
}

// R3C-7: the totals list each sub-agent once with its latest text (every turn
// lists all of the parent's sub-sessions), and keep a final_text warning only
// for the turn that is the answer.
//
// Revert-check: concatenating the turns' outputs/warnings again (the old
// append) doubles the child and keeps the stale "final_text is empty" warning.
func TestLoopTotals_DedupesSubAgentsAndDropsSupersededFinalTextWarnings(t *testing.T) {
	first := buildRunResult("s", "", "", "end_turn", nil, false, map[string]int{"agent": 1}, 10, 0.1, 0,
		"", "", 0, "", "", []SubAgentOutput{{SessionID: "child-a", FinalText: "old", CharCount: 3}}, "")
	first.Warnings = append(first.Warnings, "authoritative terminal message reconciliation failed: x")
	require.Contains(t, strings.Join(first.Warnings, "\n"), "final_text is empty after 1 sub-agent fan-out call(s)")
	second := buildRunResult("s", "the answer", "", "end_turn", nil, false, nil, 5, 0.05, 0,
		"", "", 0, "", "",
		[]SubAgentOutput{{SessionID: "child-a", FinalText: "new", CharCount: 3}, {SessionID: "child-b", FinalText: "b", CharCount: 1}}, "")

	var tot loopTotals
	tot.add(&first)
	tot.add(&second)
	tot.applyTo(&second, time.Now())
	tot.applyTo(&second, time.Now()) // idempotent

	require.Equal(t, []SubAgentOutput{
		{SessionID: "child-a", FinalText: "new", CharCount: 3},
		{SessionID: "child-b", FinalText: "b", CharCount: 1},
	}, second.SubAgentOutputs, "each child once, latest text, first-seen order")
	require.Equal(t, []string{"authoritative terminal message reconciliation failed: x"}, second.Warnings,
		"the superseded turn's final_text warning is gone, its diagnostics stay")
	require.EqualValues(t, 15, second.Usage.DeltaTokens)
}

// finalTextWarnings marks exactly the warnings that describe the turn's own
// final_text, whatever produced them.
func TestBuildRunResult_FlagsFinalTextWarnings(t *testing.T) {
	fanout := buildRunResult("s", "", "", "end_turn", nil, false, map[string]int{"agent": 2}, 0, 0, 0, "", "", 0, "", "", nil, "")
	require.Len(t, fanout.finalTextWarnings, 1)
	require.Equal(t, fanout.Warnings, fanout.finalTextWarnings)

	empty := buildRunResult("s", "", "", "end_turn", nil, false, nil, 0, 0, 0, "", "", 0, "", "", nil, "")
	require.Len(t, empty.finalTextWarnings, 1)

	tools := buildRunResult("s", "", "", "end_turn", nil, false, map[string]int{"bash": 1}, 0, 0, 0, "", "", 0, "", "", nil, "")
	require.Len(t, tools.finalTextWarnings, 1)

	truncated := buildRunResult("s", "half a sentence:", "", "error", errors.New("boom"), false, nil, 0, 0, 0, "t", "d", 0, "", "", nil, "")
	require.Len(t, truncated.finalTextWarnings, 1)
	require.Contains(t, truncated.finalTextWarnings[0], "appears truncated")

	reduced := buildRunResult("s", "short", "", "end_turn", nil, false, nil, 0, 0, 0, "", "", 0, "", "", nil, "reduction-loss: final_text is 5 chars")
	require.Equal(t, []string{"reduction-loss: final_text is 5 chars"}, reduced.finalTextWarnings)

	queued := buildRunResult("s", "", "", "", &runQueuedError{sessionID: "s"}, false, nil, 0, 0, 0, "", "", 0, "", "", nil, "")
	require.Empty(t, queued.finalTextWarnings, "the queued notice is not about final_text")
}

// R3C-8 (dedupe): nextStep re-reads the scope every time the wait wakes (a
// hint, the gate's own time), so the notice is keyed by the gate's RetryAt:
// the same gate closure prints once, the next failed attempt (a new RetryAt)
// prints again, and a refusal (no failed attempt) prints nothing here.
//
// Revert-check: dropping the RetryAt comparison prints the line on every
// wake-up.
func TestCLILoop_NoticePacedOncePerFailedAttempt(t *testing.T) {
	var stderr bytes.Buffer
	l := &cliLoop{sessionID: "s1", stderr: &stderr, streak: drainStreak{failed: errors.New("provider said no")}}
	t1 := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

	l.noticePaced(agent.CLIScopeState{Drain: agent.DrainPaced, RetryAt: t1})
	l.noticePaced(agent.CLIScopeState{Drain: agent.DrainPaced, RetryAt: t1})
	require.Equal(t, 1, strings.Count(stderr.String(), "the reaction turn failed"), "one line per gate closure")
	require.Contains(t, stderr.String(), "provider said no")
	require.Contains(t, stderr.String(), "retrying at 2026-09-30T12:00:00Z")

	l.noticePaced(agent.CLIScopeState{Drain: agent.DrainPaced, RetryAt: t1.Add(time.Minute)})
	require.Equal(t, 2, strings.Count(stderr.String(), "the reaction turn failed"), "the next failed attempt prints again")

	stderr.Reset()
	l.streak.failed = nil // a refusal clears it: afterDrain prints its own line
	l.noticePaced(agent.CLIScopeState{Drain: agent.DrainPaced, RetryAt: t1.Add(2 * time.Minute)})
	require.Empty(t, stderr.String())
}
