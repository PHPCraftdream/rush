// Round-8 fixes of the `rush run` loop (docs/reviews/2026-09-30-async-phase4-
// round8.md, W8-SHELL: R8A-1, R8B-3, R8A-2 loop part, R8C-7): same harness as
// app_run_loop_test.go.
package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// onWrite runs fn (once) the moment the loop prints substr: a deterministic
// probe of the state "while the loop waits".
type onWrite struct {
	syncBuffer
	substr string
	fn     func()
	once   sync.Once
}

func (w *onWrite) Write(p []byte) (int, error) {
	n, err := w.syncBuffer.Write(p)
	if bytes.Contains(p, []byte(w.substr)) {
		w.once.Do(w.fn)
	}
	return n, err
}

func sessionEndedReason(t *testing.T, application *App, sessionID string) string {
	t.Helper()
	sess, err := application.Sessions.Get(context.Background(), sessionID)
	require.NoError(t, err)
	return sess.EndedReason
}

// R8A-1: "ended_reason is empty while a run is in progress". A `rush run` loop
// that waits on a job after its first turn used to show the first turn's
// "end_turn" (ExecuteRun's per-turn write), so `sessions show` / `list --json`
// called a live session finished. Loop-driven turns no longer write it; the
// loop's exit writes the envelope's reason once.
//
// Revert-check: dropping the loopTurn guard in ExecuteRun's ended_reason defer
// leaves "end_turn" while the loop waits and adds a write to the log.
func TestRunLoop_EndedReasonStaysEmptyWhileTheLoopWaits(t *testing.T) {
	h := r6HeldFirstJobHarness(t)
	writes := logEndedReasonWrites(t, h.app)
	ctx, cancel := context.WithCancel(loopCtx(t))
	var whileWaiting string
	stderr := &onWrite{substr: "still has open work", fn: func() {
		whileWaiting = sessionEndedReason(t, h.app, h.sessionID)
		cancel()
	}}
	cliLoopStderr = stderr
	t.Cleanup(func() { cliLoopStderr = nil })

	res, _, err := h.run(ctx, RunOverrides{})

	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, res)
	require.Empty(t, whileWaiting, "the row says nothing about how the run ended while the loop still waits")
	require.Equal(t, "canceled", res.ExitReason)
	require.Equal(t, res.ExitReason, sessionEndedReason(t, h.app, h.sessionID), "the exit writes the envelope's reason")
	require.Equal(t, []string{"", "canceled"}, writes(), "the start clear, then the loop's one exit write")
}

// R8A-1: the same holds across Drain turns and for the ordinary clean exit:
// the row is empty during every turn (a Drain's own provider request included)
// and the loop's exit is the only writer, so a run with N turns writes once.
//
// Revert-check: as above -- the Drain's per-turn write is visible in the
// provider handler and in the log.
func TestRunLoop_OnlyTheExitWritesEndedReason(t *testing.T) {
	var duringDrain atomic.Value
	var h *loopHarness
	h = newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
		if drain {
			duringDrain.Store(sessionEndedReason(t, h.app, h.sessionID))
			loopText(w, "d", "R8-REACTION", 30, 10)
			return
		}
		loopText(w, "a", "R8-FIRST", 11, 3)
	})
	writes := logEndedReasonWrites(t, h.app)
	h.afterFirstTurn(h.seedDebt)

	res, _, err := h.run(loopCtx(t), RunOverrides{})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.EqualValues(t, 1, h.drains.Load(), "the Drain ran")
	require.Equal(t, "", duringDrain.Load(), "the first turn left the row empty for the Drain")
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, "end_turn", sessionEndedReason(t, h.app, h.sessionID))
	require.Equal(t, []string{"", "end_turn"}, writes(), "one write for the whole run")
}

// R8A-1: the reviewer pass is a loop turn too: a run that goes first turn,
// Drain, reviewer turn writes ended_reason ONCE, at the exit, with the
// verdict's reason -- not once per turn.
//
// Revert-check: dropping `loopTurn: true` from runReviewerTurn (or from
// runTurn) adds the turn's own write to the log.
func TestRunLoop_ReviewerPassWritesEndedReasonOnlyAtTheExit(t *testing.T) {
	rh := newReviewerHarness(t, true)
	writes := logEndedReasonWrites(t, rh.app)
	var afterTurns []string // the row as each turn returns: first, Drain, reviewer
	cliLoopTurnDoneSeam = func() { afterTurns = append(afterTurns, sessionEndedReason(t, rh.app, rh.sessionID)) }
	t.Cleanup(func() { cliLoopTurnDoneSeam = nil })

	res, _, err := rh.run(loopCtx(t), RunOverrides{ModelRole: config.SelectedModelTypeSmart})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotEmpty(t, rh.reviewerBodies(), "the reviewer turn ran")
	require.Equal(t, []string{"", "", ""}, afterTurns, "no turn of the loop leaves a reason behind")
	require.Equal(t, r3ReactionText, res.FinalText, "A10: final_text stays the executor's answer")
	require.Equal(t, r3ReviewVerdict, res.Review, "the verdict is the additive review field")
	require.EqualValues(t, 3, rh.requests.Load(), "first turn, one Drain, one reviewer turn")
	require.Equal(t, []string{"", res.ExitReason}, writes(), "one write for first turn, Drain and reviewer turn together")
	require.Equal(t, res.ExitReason, sessionEndedReason(t, rh.app, rh.sessionID))
}

// R8A-1 (non-loop half of the contract): a caller that is not the CLI loop --
// the SDK, ExecuteRun directly -- has no loop exit to write for it, so its
// per-turn write stays.
//
// Revert-check: suppressing the write for every caller leaves the row empty.
func TestExecuteRun_NonLoopCallerKeepsThePerTurnEndedReason(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "R8-SDK", 11, 3)
	})
	writes := logEndedReasonWrites(t, h.app)

	res, err := h.app.ExecuteRun(loopCtx(t), RunRequest{
		Prompt: "do it", Mode: RunModeJSON, ContinueSessionID: h.sessionID,
		Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true, captureResult: true,
	})

	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "end_turn", sessionEndedReason(t, h.app, h.sessionID))
	require.Equal(t, []string{"", "end_turn"}, writes())
}

// r8WakeSource answers CLIScope with a fixed state once armed (after the first
// turn) and returns from WaitForHint at once; onScope runs at every armed
// CLIScope call, standing in for what happens between two wakes.
type r8WakeSource struct {
	agent.ReactionDebtSource
	armed   atomic.Bool
	state   agent.CLIScopeState
	onScope func(n int32)
	scopes  atomic.Int32
}

func (s *r8WakeSource) CLIScope(ctx context.Context, sessionID string) (agent.CLIScopeState, error) {
	if !s.armed.Load() {
		return s.ReactionDebtSource.CLIScope(ctx, sessionID)
	}
	n := s.scopes.Add(1)
	if s.onScope != nil {
		s.onScope(n)
	}
	return s.state, nil
}

func (s *r8WakeSource) WaitForHint(ctx context.Context, sessionID string, until time.Time) {
	if !s.armed.Load() {
		s.ReactionDebtSource.WaitForHint(ctx, sessionID, until)
		return
	}
	sleepOrCtxDone(ctx, time.Millisecond)
}

// R8B-3: `--max-cost` bounds the run while the loop waits on running work or
// on the launch gate. A delegated child's Drains charge the root between
// wakes; before the fix nothing read the root's cost until the delegation
// released, so the chain ran to the 6 h cap. Each fake wake here charges $0.30
// against `--max-cost 0.50`: the loop exits "error" (cap) at the second wake,
// through exitPrecheck, before anything further runs.
//
// Revert-check: without the stop check in nextStep's WorkOpen / Paced branch
// the loop keeps waiting until the test's deadline and exits "canceled".
func TestRunLoop_MaxCostWhileWaitingEndsTheRun(t *testing.T) {
	states := map[string]agent.CLIScopeState{
		"running work": {WorkOpen: true},
		"paced gate":   {Drain: agent.DrainPaced, RetryAt: time.Now().Add(time.Hour)},
	}
	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
				loopText(w, "a", "R8-FIRST", 11, 3)
			})
			src := &r8WakeSource{ReactionDebtSource: driverSource(t, h.app), state: state}
			src.onScope = func(int32) {
				_, err := h.app.Sessions.IncrementCost(context.Background(), h.sessionID, 0.30)
				require.NoError(t, err)
			}
			h.afterFirstTurn(func() { src.armed.Store(true) })
			var out, stderr syncBuffer
			overrides := RunOverrides{Origin: message.OriginCLI, MaxCost: 0.50}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			l := &cliLoop{
				app: h.app, source: src, ctx: ctx, output: &out, mode: RunModeJSON, hideSpinner: true,
				overrides: overrides, turnOverrides: overrides, prompt: "do it",
				continueSessionID: h.sessionID, started: time.Now(), sessionID: h.sessionID,
				lastBuffered: &bytes.Buffer{}, stderr: &stderr,
			}

			res, err := l.run()

			var inc *runIncompleteError
			require.ErrorAs(t, err, &inc)
			require.Equal(t, "error", inc.reason)
			require.Contains(t, inc.detail, "exceeds max")
			require.NotNil(t, res)
			require.Equal(t, "error", res.ExitReason)
			require.Contains(t, out.String(), `"exit_reason":"error"`)
			require.EqualValues(t, 2, src.scopes.Load(), "the cap is seen at the wake that crossed it, not later")
			require.EqualValues(t, 0, h.drains.Load(), "no reaction turn is launched past the cap")
			require.Equal(t, "error", sessionEndedReason(t, h.app, h.sessionID))
		})
	}
}

// R8A-2 / R8C-2 (loop part): a cancel request the loop HONOURS is one-shot --
// the flag is cleared before the exit flush, so a later web turn or takeover
// Drain on the same session is not aborted after its first step.
//
// Revert-check: without clearHonouredCancel the flag survives both exits.
func TestRunLoop_HonouredCancelIsCleared(t *testing.T) {
	requireCleared := func(t *testing.T, h *loopHarness) {
		t.Helper()
		canceled, err := h.app.Sessions.IsCancelRequested(context.Background(), h.sessionID)
		require.NoError(t, err)
		require.False(t, canceled, "the flag was honoured, so it must not outlive the run")
	}
	t.Run("while waiting on running work", func(t *testing.T) {
		h := r6HeldFirstJobHarness(t)
		stderr := &requestCancelOnWrite{substr: "still has open work"}
		stderr.cancel = func() { require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID)) }
		cliLoopStderr = stderr
		t.Cleanup(func() { cliLoopStderr = nil })

		res, _, err := h.run(r7RunCtx(t), RunOverrides{})

		var inc *runIncompleteError
		require.ErrorAs(t, err, &inc)
		require.Equal(t, "canceled", inc.reason)
		require.NotNil(t, res)
		requireCleared(t, h)
	})
	t.Run("at the precheck before a Drain", func(t *testing.T) {
		h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, drain bool, _ int) {
			if drain {
				t.Error("no Drain may be launched after `sessions cancel`")
			}
			loopText(w, "a", "R8-FIRST", 11, 3)
		})
		h.afterFirstTurn(func() {
			h.seedDebt()
			require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
		})

		res, _, err := h.run(loopCtx(t), RunOverrides{})

		var inc *runIncompleteError
		require.ErrorAs(t, err, &inc)
		require.Equal(t, "canceled", inc.reason)
		require.NotNil(t, res)
		requireCleared(t, h)
	})
}

// R8A-2 / R8C-2 (loop part): a cancel that lands DURING the first turn (which
// starts the job the loop then waits on) ends the run canceled and is cleared
// too. This also pins the interplay with the agent's in-turn abort: that abort
// must leave the flag for the loop to see (the loop is the one that ends the
// run and waits on the started job; a flag consumed by the turn would let the
// loop wait for the held job until the deadline).
//
// Revert-check: without clearHonouredCancel the flag survives.
func TestRunLoop_CancelDuringFirstTurnEndsCanceledAndClearsTheFlag(t *testing.T) {
	gate := t.TempDir() + "/gate"
	var h *loopHarness
	h = newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, body []byte, _ bool, n int) {
		_, _, lastTool := lastTurnParts(body)
		if strings.Contains(lastTool, "Async bash job") {
			loopText(w, "wait", "R8-WAITING", 30, 6)
			return
		}
		if n == 1 {
			require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
		}
		command := `until [ -f "` + gate + `" ]; do :; done`
		admissionWriteSSE(w, []string{
			admissionSSEToolCall("rb", "call-bash", "bash", `{"command":`+jsonString(command)+`,"description":"hold"}`),
			admissionSSEStop("rb", "tool_calls"),
		})
	})

	res, _, err := h.run(r7RunCtx(t), RunOverrides{})

	require.Error(t, err)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	canceled, cErr := h.app.Sessions.IsCancelRequested(context.Background(), h.sessionID)
	require.NoError(t, cErr)
	require.False(t, canceled)
}

// R8A-1 / R8A-2 (loop part): a refused run ran nothing. A first turn refused
// because another process holds the session ends the run with the busy error;
// the exit records no ended_reason on the owner's live session (it used to
// write "error" there) and a cancel request that landed meanwhile is the
// owner's, so it survives. (The refused turn's own setup already clears the
// flag and the reason once, before the refusal -- an older gap -- so the
// request is planted right after the turn returned.)
//
// Revert-check: dropping refusedByOwner from endedReasonFor writes "error";
// clearing the flag on every loop exit turns the second assertion red.
func TestRunLoop_LockBusyRefusalLeavesTheOwnersSessionAlone(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, _ http.ResponseWriter, _ []byte, _ bool, _ int) {
		t.Error("provider must never be called: the lock refusal is caught before any turn runs")
	})
	foreign, err := session.TryAcquireSessionLock(h.dataDir, h.sessionID)
	require.NoError(t, err)
	t.Cleanup(func() { _ = foreign.Release() })
	writes := logEndedReasonWrites(t, h.app)
	h.afterFirstTurn(func() {
		require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
	})

	_, _, runErr := h.run(loopCtx(t), RunOverrides{})

	var lockBusy *session.SessionLockBusyError
	require.ErrorAs(t, runErr, &lockBusy)
	require.NotContains(t, writes(), "error", "the refusal ends no run of the owner's")
	canceled, cErr := h.app.Sessions.IsCancelRequested(context.Background(), h.sessionID)
	require.NoError(t, cErr)
	require.True(t, canceled, "the refused run must not consume the owner's cancel request")
}

// epipeWriter fails every write the way a closed stdout pipe does.
type epipeWriter struct{}

func (epipeWriter) Write([]byte) (int, error) {
	return 0, &os.PathError{Op: "write", Path: "/dev/stdout", Err: syscall.EPIPE}
}

// R8C-7: when the exit flush fails because stdout is a closed pipe (what the
// command's SIGPIPE handling turns the fatal signal into), the run still ends
// through its ordinary error path: the error names EPIPE, --on-finish ran with
// the final envelope's data, and the loop's driver marker is released, so the
// caller's Shutdown runs next. The hook also sees the envelope: the session id
// and the exit reason.
//
// Revert-check: dropping the on-finish call from the loop's deferred hook (or
// returning before it on a flush error) leaves no hook file.
func TestRunLoop_ExitFlushOnClosedPipeStillRunsTheHook(t *testing.T) {
	for name, mode := range map[string]RunMode{"json": RunModeJSON, "terse": RunModeTerse} {
		t.Run(name, func(t *testing.T) {
			h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
				loopText(w, "a", "R8-FIRST", 11, 3)
			})
			marker := filepath.Join(t.TempDir(), "hook.txt")
			hook := "echo $RUSH_SESSION_ID:$RUSH_EXIT_REASON > " + marker
			if runtime.GOOS == "windows" {
				hook = "echo %RUSH_SESSION_ID%:%RUSH_EXIT_REASON%> " + marker
			}

			res, err := h.app.RunNonInteractiveWithResult(loopCtx(t), epipeWriter{}, "do it",
				RunOverrides{Origin: message.OriginCLI, OnFinishHook: hook}, true, mode, h.sessionID, false)

			require.ErrorIs(t, err, syscall.EPIPE)
			require.NotNil(t, res)
			data, readErr := os.ReadFile(marker)
			require.NoError(t, readErr, "--on-finish still runs when the flush fails")
			require.Contains(t, string(data), h.sessionID+":end_turn")
			drivers, drvErr := h.app.LiveSessionDrivers(context.Background())
			require.NoError(t, drvErr)
			require.NotContains(t, drivers, h.sessionID, "the driver marker is released on the flush-error exit")
			require.Equal(t, "end_turn", sessionEndedReason(t, h.app, h.sessionID), "the reason is recorded before the flush")
		})
	}
}
