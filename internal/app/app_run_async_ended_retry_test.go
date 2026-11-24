package app

// FIX-STATUS (b) regression tests: the loop's ended_reason write used to be
// ONE best-effort attempt on a 2s budget; a concurrent-exit burst contending
// for the SQLite write lock outlasted it ("context deadline exceeded"), the
// envelope had already been flushed, and nothing retried -- the row kept an
// empty ended_reason forever.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/stretchr/testify/require"
)

// flakyEndedReasonSet fails the first N SetEndedReason calls, then passes
// through to the wrapped service.
type flakyEndedReasonSet struct {
	session.Service
	calls atomic.Int32
	fail  atomic.Int32
}

func (f *flakyEndedReasonSet) SetEndedReason(ctx context.Context, id, reason string) error {
	if f.calls.Add(1) <= f.fail.Load() {
		return context.DeadlineExceeded
	}
	return f.Service.SetEndedReason(ctx, id, reason)
}

// TestPersistEndedReason_RetriesAfterATransientWriteFailure: a first failed
// write must not be the last word -- the second attempt records the reason.
//
// Revert-check: removing the retry (one attempt per budget) leaves the row
// empty and fails the ended_reason assertion.
func TestPersistEndedReason_RetriesAfterATransientWriteFailure(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "R8-X", 1, 1)
	})
	svc := &flakyEndedReasonSet{Service: h.app.Sessions}
	svc.fail.Store(1)
	h.app.Sessions = svc

	l := &cliLoop{env: h.app.runEnv(), host: h.app.loopHost(), ctx: t.Context(), sessionID: h.sessionID, firstSubmitted: true}
	l.persistEndedReason(&RunResult{SessionID: h.sessionID, ExitReason: "end_turn"}, nil)

	require.EqualValues(t, 2, svc.calls.Load(), "the failed first write must be retried exactly once more")
	require.Equal(t, "end_turn", sessionEndedReason(t, h.app, h.sessionID))
}

// TestPersistEndedReason_GivesUpAfterBoundedAttempts: an always-failing
// store stops after exactly endedReasonWriteAttempts calls -- bounded work,
// not an endless loop, and the final Warn keeps the last error.
//
// Revert-check: an unbounded retry never returns (the package would hang
// until the go test deadline); an attempt-count regression fails the
// equality.
func TestPersistEndedReason_GivesUpAfterBoundedAttempts(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "R8-X", 1, 1)
	})
	svc := &flakyEndedReasonSet{Service: h.app.Sessions}
	svc.fail.Store(1 << 30)
	h.app.Sessions = svc

	l := &cliLoop{env: h.app.runEnv(), host: h.app.loopHost(), ctx: t.Context(), sessionID: h.sessionID, firstSubmitted: true}
	l.persistEndedReason(&RunResult{SessionID: h.sessionID, ExitReason: "end_turn"}, nil)

	require.EqualValues(t, endedReasonWriteAttempts, svc.calls.Load())
	require.Empty(t, sessionEndedReason(t, h.app, h.sessionID),
		"an always-failing store leaves the recorded reason empty; the Warn is the only trace")
}

type reportNoReplayTestError struct{}

// The existing loop harness uses an HTTP-compatible provider fixture, not the
// Codex Responses WebSocket transport. This host error exercises recovery logic
// only; it does not claim provider-wire NoReplay coverage.
func (reportNoReplayTestError) Error() string  { return "sent stream failed" }
func (reportNoReplayTestError) NoReplay() bool { return true }

func TestReportRecoveryAttemptSkipsTodoReminderAfterFailure(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Fatal("host-only epilogue test must not call the provider fixture")
	})
	require.NoError(t, h.app.Sessions.SetTodos(t.Context(), h.sessionID, []session.Todo{{Content: "unfinished", Status: session.TodoStatusPending}}, nil))
	originalErr := fmt.Errorf("original provider failure: %w", reportNoReplayTestError{})
	originalFinal := &RunResult{SessionID: h.sessionID, ExitReason: "error", Error: originalErr.Error(), ToolCalls: []ToolCallStat{{Name: "edit", Count: 1}}}
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID, failReport: true, reportErr: errors.New("distinct report failure")}
	var output bytes.Buffer
	l := &cliLoop{env: h.app.runEnv(), host: host, source: driverSource(t, h.app), ctx: t.Context(), sessionID: h.sessionID, output: &output, mode: RunModeJSON, reportRetryRemaining: 1, final: originalFinal, runErr: originalErr, lastFailed: originalFinal}
	l.tot.add(originalFinal)
	final, err := l.finish(originalErr)
	require.ErrorIs(t, err, originalErr)
	require.Equal(t, originalFinal.Usage, final.Usage)
	require.Equal(t, originalFinal.ToolCalls, final.ToolCalls)
	require.NotContains(t, final.Warnings, "failed-turn warning", "failed recovery warnings must not leak into the original run")
	require.Len(t, host.requests, 1)
	require.True(t, host.requests[0].reportTurn)
	require.Len(t, l.tot.turns, 1, "failed recovery must not contribute to final run totals")
	open, _, todoErr := l.openTodos()
	require.NoError(t, todoErr)
	require.Equal(t, 1, open)
	require.False(t, l.todoNudgeDue())
	require.Contains(t, final.Warnings, "report recovery failed: distinct report failure")
	require.NotNil(t, l.output)
	require.NotEmpty(t, output.Bytes())
}

type reportRecoveryHost struct {
	LoopHost
	sessionID  string
	requests   []RunRequest
	failReport bool
	failFirst  bool
	failNudge  bool
	nudgeErr   error
	firstErr   error
	reportErr  error
}

func (h *reportRecoveryHost) executeRun(ctx context.Context, req RunRequest) (*RunResult, error) {
	h.requests = append(h.requests, req)
	failed := req.reportTurn && h.failReport || req.nudgeTurn && h.failNudge || !req.reportTurn && !req.reviewerTurn && !req.nudgeTurn && h.failFirst
	if req.onSessionResolved != nil {
		if err := req.onSessionResolved(h.sessionID); err != nil {
			return nil, err
		}
	}
	if req.onTurnSubmitted != nil {
		req.onTurnSubmitted()
	}
	if failed {
		cause := fmt.Errorf("synthetic sent-stream failure: %w", reportNoReplayTestError{})
		if req.reportTurn && h.reportErr != nil {
			cause = h.reportErr
		} else if req.nudgeTurn && h.nudgeErr != nil {
			cause = h.nudgeErr
		} else if h.firstErr != nil && !req.reportTurn && !req.nudgeTurn {
			cause = h.firstErr
		}
		return &RunResult{
			SessionID: h.sessionID,
			Usage:     UsageInfo{DeltaTokens: 7, DeltaCostUSD: 0.25},
			ToolCalls: []ToolCallStat{{Name: "edit", Count: 1}},
			Warnings:  []string{"failed-turn warning"},
		}, cause
	}
	if req.reportTurn {
		result := &RunResult{SessionID: h.sessionID, FinalText: "RECOVERED-REPORT", ToolCalls: []ToolCallStat{}}
		if req.Stdout != nil {
			_, _ = io.WriteString(req.Stdout, result.FinalText)
		}
		return result, nil
	}
	text := "successful turn"
	if req.reviewerTurn {
		text = "REVIEWER-ANSWER"
	}
	result := &RunResult{SessionID: h.sessionID, FinalText: text, ToolCalls: []ToolCallStat{}}
	if req.Stdout != nil {
		_, _ = io.WriteString(req.Stdout, result.FinalText)
	}
	return result, nil
}

func TestFirstTurnPhaseRecoversNoReplayReport(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Fatal("LoopHost owns all requests in this host-only regression")
	})
	var output bytes.Buffer
	source := driverSource(t, h.app)
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID, failFirst: true}
	h.app.config.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: "reviewer-probe"})
	l := &cliLoop{
		env: h.app.runEnv(), host: host, source: source, ctx: t.Context(),
		sessionID: h.sessionID, output: &output, mode: RunModeTerse, reportRetryRemaining: 1,
		overrides: RunOverrides{ModelRole: config.SelectedModelTypeSmart},
	}

	step := l.firstTurnPhase()
	require.Equal(t, evFirstContinue, step.ev)
	final, err := l.finish(l.runErr)
	require.Error(t, err)
	require.Len(t, host.requests, 3)
	require.True(t, host.requests[1].reportTurn)
	require.True(t, host.requests[2].reviewerTurn)
	require.True(t, l.reviewerDone)
	require.Equal(t, "RECOVERED-REPORT", final.FinalText)
	require.True(t, final.Recovered)
	require.Equal(t, "RECOVERED-REPORT", output.String(), "terse output must retain the recovered report, not reviewer text")
}

func TestPendingDrainFailureDoesNotConsumeRecovery(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Fatal("pending Drain must not reach provider fixture")
	})
	require.NoError(t, h.app.Sessions.SetTodos(t.Context(), h.sessionID, []session.Todo{{Content: "pending debt", Status: session.TodoStatusPending}}, nil))
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID, failFirst: true}
	source := &scriptedScopeSource{script: []scopeAnswer{{state: agent.CLIScopeState{Drain: agent.DrainOwed}}}}
	l := &cliLoop{env: h.app.runEnv(), host: host, source: source, ctx: t.Context(), sessionID: h.sessionID, reportRetryRemaining: 1}
	result, err := host.executeRun(t.Context(), RunRequest{drainTurn: true})
	done, afterErr := l.afterDrain(result, err, nil, usageMark{})
	require.False(t, done)
	require.NoError(t, afterErr)
	step := l.decidePhase()
	require.Equal(t, evScopeDrain, step.ev, "the owed reaction must be drained before any exit or recovery")
	require.Len(t, host.requests, 1)
	require.False(t, l.reportRecoveryTried)
	require.Equal(t, 1, l.reportRetryRemaining)
}

func TestTodoNudgeNoReplayFailureRecoversWhenNoAnswerExists(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) { t.Fatal("host-only test") })
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID, failNudge: true}
	var output bytes.Buffer
	originalErr := fmt.Errorf("nudge failed: %w", reportNoReplayTestError{})
	l := &cliLoop{
		env: h.app.runEnv(), host: host, source: driverSource(t, h.app), ctx: t.Context(), sessionID: h.sessionID, output: &output, mode: RunModeJSON, reportRetryRemaining: 1,
		final: &RunResult{SessionID: h.sessionID, ToolCalls: []ToolCallStat{{Name: "edit", Count: 1}}}, runErr: originalErr,
	}
	step := l.nudgePhase()
	require.Equal(t, evNudgeAgain, step.ev)
	final, err := l.finish(originalErr)
	require.ErrorIs(t, err, originalErr)
	require.Len(t, host.requests, 2)
	require.True(t, host.requests[0].nudgeTurn)
	require.True(t, host.requests[1].reportTurn)
	require.True(t, l.recoveredReport)
	require.Equal(t, "RECOVERED-REPORT", final.FinalText)
	require.Equal(t, originalErr.Error(), final.Error)
	require.NotEmpty(t, output.Bytes())
}

func TestTodoNudgeOrdinaryFailureIsDropped(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) { t.Fatal("host-only test") })
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID, failNudge: true, nudgeErr: errors.New("ordinary nudge failure")}
	l := &cliLoop{
		env: h.app.runEnv(), host: host, source: driverSource(t, h.app), ctx: t.Context(), sessionID: h.sessionID, reportRetryRemaining: 1,
		final: &RunResult{SessionID: h.sessionID, ToolCalls: []ToolCallStat{{Name: "edit", Count: 1}}},
	}
	step := l.nudgePhase()
	require.Equal(t, evNudgeAgain, step.ev)
	require.Nil(t, l.runErr, "ordinary nudge failures retain their historical dropped behavior")
	require.False(t, l.reportRecoveryTried)
	require.Len(t, host.requests, 1)
}

func TestTerminalNoToolCallsDoesNotRecoverReport(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Fatal("ineligible failure must not call provider")
	})
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID}
	var output bytes.Buffer
	originalErr := fmt.Errorf("no calls: %w", reportNoReplayTestError{})
	l := &cliLoop{
		env: h.app.runEnv(), host: host, ctx: t.Context(), sessionID: h.sessionID, output: &output, mode: RunModeJSON, reportRetryRemaining: 1,
		final: &RunResult{SessionID: h.sessionID, ExitReason: "error", Error: originalErr.Error(), ToolCalls: []ToolCallStat{}}, runErr: originalErr,
	}
	final, err := l.exit(originalErr, "")
	require.ErrorIs(t, err, originalErr)
	require.Empty(t, host.requests)
	require.False(t, l.reportRecoveryTried)
	require.Equal(t, originalErr.Error(), final.Error)
	require.NotEmpty(t, output.Bytes())
}

func TestTerminalFailureRecoversReport(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) { t.Fatal("host-only test") })
	originalErr := fmt.Errorf("terminal failure: %w", reportNoReplayTestError{})
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID}
	var output bytes.Buffer
	l := &cliLoop{
		env: h.app.runEnv(), host: host, source: driverSource(t, h.app), ctx: t.Context(), sessionID: h.sessionID, output: &output, mode: RunModeJSON, reportRetryRemaining: 1,
		final: &RunResult{SessionID: h.sessionID, ExitReason: "error", Error: originalErr.Error(), ToolCalls: []ToolCallStat{{Name: "edit", Count: 1}}}, runErr: originalErr,
	}
	final, err := l.exit(originalErr, "")
	require.ErrorIs(t, err, originalErr)
	require.True(t, l.recoveredReport)
	require.Equal(t, "RECOVERED-REPORT", final.FinalText)
	require.NotEmpty(t, output.Bytes())
}

func TestTerminalFailureWithoutFinalRecoversReport(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) { t.Fatal("host-only test") })
	originalErr := fmt.Errorf("terminal failure: %w", reportNoReplayTestError{})
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID}
	var output bytes.Buffer
	failed := &RunResult{SessionID: h.sessionID, ExitReason: "error", ToolCalls: []ToolCallStat{{Name: "edit", Count: 1}}}
	l := &cliLoop{
		env: h.app.runEnv(), host: host, source: driverSource(t, h.app), ctx: t.Context(), sessionID: h.sessionID, output: &output, mode: RunModeJSON, reportRetryRemaining: 1,
		runErr: originalErr, lastFailed: failed,
	}
	l.tot.add(failed) // afterDrain accounts for this failed turn in production.

	final, err := l.finish(originalErr)

	require.ErrorIs(t, err, originalErr)
	require.True(t, l.recoveredReport)
	require.Equal(t, "RECOVERED-REPORT", final.FinalText)
	require.Equal(t, "error", final.ExitReason)
	require.Equal(t, originalErr.Error(), final.Error)
	require.Equal(t, []ToolCallStat{{Name: "edit", Count: 1}}, final.ToolCalls)
	require.Equal(t, "", failed.FinalText, "the failed result must remain unchanged")
	require.Len(t, host.requests, 1)
	require.NotEmpty(t, output.Bytes())
}

func TestDrainFailureDoesNotReplaceExistingFinalAnswer(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) { t.Fatal("host-only test") })
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID}
	originalErr := fmt.Errorf("provider: %w", reportNoReplayTestError{})
	var output bytes.Buffer
	l := &cliLoop{
		env: h.app.runEnv(), host: host, ctx: t.Context(), sessionID: h.sessionID, output: &output, mode: RunModeJSON, reportRetryRemaining: 1,
		final: &RunResult{SessionID: h.sessionID, FinalText: "already answered", ToolCalls: []ToolCallStat{{Name: "edit", Count: 1}}}, runErr: originalErr,
		lastFailed: &RunResult{SessionID: h.sessionID, ToolCalls: []ToolCallStat{{Name: "edit", Count: 1}}},
	}
	final, err := l.finish(originalErr)
	require.Error(t, err)
	require.Empty(t, host.requests)
	require.Equal(t, "already answered", final.FinalText)
	require.NotEmpty(t, output.Bytes())
	require.False(t, l.reportRecoveryTried)
}

func TestFirstTurnPhaseDoesNotRecoverWithZeroRetries(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Fatal("zero retries must not send a recovery request")
	})
	host := &reportRecoveryHost{LoopHost: h.app.loopHost(), sessionID: h.sessionID, failFirst: true}
	var output bytes.Buffer
	l := &cliLoop{env: h.app.runEnv(), host: host, source: driverSource(t, h.app), ctx: t.Context(), sessionID: h.sessionID, output: &output, mode: RunModeJSON, reportRetryRemaining: 0}
	step := l.firstTurnPhase()
	require.Equal(t, evFirstContinue, step.ev)
	final, runErr := l.finish(l.runErr)
	require.Error(t, runErr)
	require.Equal(t, "error", final.ExitReason)
	require.Len(t, host.requests, 1)
	require.False(t, l.reportRecoveryTried)
	require.NotEmpty(t, output.Bytes())
}

func TestReportTurnFailureKeepsFailureAndDoesNotClaimRecovery(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Fatal("failed recovery must not send another provider request")
	})
	originalErr := fmt.Errorf("original provider failure: %w", reportNoReplayTestError{})
	recoveryErr := errors.New("distinct recovery report failure")
	host := &reportRecoveryHost{
		LoopHost: h.app.loopHost(), sessionID: h.sessionID, failFirst: true, failReport: true,
		firstErr: originalErr, reportErr: recoveryErr,
	}
	var output bytes.Buffer
	l := &cliLoop{env: h.app.runEnv(), host: host, source: driverSource(t, h.app), ctx: t.Context(), sessionID: h.sessionID, output: &output, mode: RunModeJSON, reportRetryRemaining: 1}

	final, runErr := l.run()

	require.ErrorIs(t, runErr, originalErr)
	require.Equal(t, originalErr.Error(), runErr.Error())
	require.Len(t, host.requests, 2)
	require.False(t, l.recoveredReport)
	require.Equal(t, "error", final.ExitReason)
	require.Equal(t, originalErr.Error(), final.Error, "recovery error must not replace the original provider failure")
	require.NotEmpty(t, output.Bytes())
	require.Contains(t, final.Warnings, "report recovery failed: distinct recovery report failure")
	require.Equal(t, 1, countWarning(final.Warnings, "report recovery failed after a non-replayable provider stream failure"))
}

func countWarning(warnings []string, want string) int {
	count := 0
	for _, warning := range warnings {
		if warning == want {
			count++
		}
	}
	return count
}

func TestShouldRecoverEmptyReport(t *testing.T) {
	cause := fmt.Errorf("provider failed: %w", reportNoReplayTestError{})
	withCalls := &RunResult{FinalText: "", ToolCalls: []ToolCallStat{{Name: "edit", Count: 2}}}
	withoutCalls := &RunResult{FinalText: "", ToolCalls: []ToolCallStat{}}
	for _, tc := range []struct {
		name      string
		result    *RunResult
		err       error
		remaining int
		want      bool
	}{
		{"eligible", withCalls, cause, 1, true},
		{"accumulated calls", &RunResult{FinalText: "", ToolCalls: []ToolCallStat{}}, cause, 1, true},
		{"zero retries", withCalls, cause, 0, false},
		{"no tool calls", withoutCalls, cause, 1, false},
		{"nonempty answer", &RunResult{FinalText: "partial", ToolCalls: withCalls.ToolCalls}, cause, 1, false},
		{"ordinary error", withCalls, errors.New("ordinary"), 1, false},
		{"nil result", nil, cause, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &cliLoop{reportRetryRemaining: tc.remaining}
			if tc.name == "accumulated calls" {
				l.tot.counts = map[string]int{"edit": 2}
			}
			require.Equal(t, tc.want, l.shouldRecoverEmptyReport(tc.result, tc.err))
		})
	}
}

// TestWriteEndedReasonWithRetry_StopsOnFirstSuccess: the retry loop returns
// nil on the first successful write instead of exhausting all attempts.
//
// Revert-check: retrying past a success fails the calls assertion; dropping
// the retry fails the error assertion instead.
func TestWriteEndedReasonWithRetry_StopsOnFirstSuccess(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	err := writeEndedReasonWithRetry(context.Background(), func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
}

// TestWriteEndedReasonWithRetry_FreshBudgetPerAttempt: each attempt's
// context carries its own live deadline; a parent whose budget is already
// spent must not hand an exhausted context to the write.
//
// Revert-check: deriving the attempt context from the parent without
// WithoutCancel plus a fresh WithTimeout (the one-shared-budget regression)
// fails the Err/Deadline assertions on the first attempt.
func TestWriteEndedReasonWithRetry_FreshBudgetPerAttempt(t *testing.T) {
	spent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-spent.Done() // the parent budget is already exhausted

	var attempts atomic.Int32
	err := writeEndedReasonWithRetry(spent, func(ctx context.Context) error {
		attempts.Add(1)
		require.Nil(t, ctx.Err(), "an attempt context must not start exhausted")
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "each attempt must carry a deadline")
		require.Greater(t, time.Until(deadline), time.Duration(0), "the fresh budget must be in the future")
		return errors.New("boom")
	})
	require.Error(t, err)
	require.EqualValues(t, endedReasonWriteAttempts, attempts.Load())
}
