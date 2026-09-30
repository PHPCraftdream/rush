// Round-6 fixes of the `rush run` loop (docs/reviews/2026-09-30-async-phase4-
// round6.md, W6-APP: R6C-4, R6C-6): same harness as app_run_loop_test.go (real
// App and SQLite, httptest provider).
package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// logEndedReasonWrites records every write of the session row's ended_reason,
// in order (SetEndedReason is the only writer of the column).
func logEndedReasonWrites(t *testing.T, application *App) func() []string {
	t.Helper()
	ctx := context.Background()
	_, err := application.DB().ExecContext(ctx, `CREATE TABLE fx_ended_log (id INTEGER PRIMARY KEY AUTOINCREMENT, reason TEXT)`)
	require.NoError(t, err)
	_, err = application.DB().ExecContext(ctx, `CREATE TRIGGER fx_ended_log_t AFTER UPDATE OF ended_reason ON sessions
		BEGIN INSERT INTO fx_ended_log(reason) VALUES (NEW.ended_reason); END`)
	require.NoError(t, err)
	return func() []string {
		rows, qErr := application.DB().QueryContext(ctx, `SELECT reason FROM fx_ended_log ORDER BY id`)
		require.NoError(t, qErr)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var reason string
			require.NoError(t, rows.Scan(&reason))
			out = append(out, reason)
		}
		require.NoError(t, rows.Err())
		return out
	}
}

func endedReasonOf(t *testing.T, application *App, sessionID string) string {
	t.Helper()
	var reason string
	require.NoError(t, application.DB().QueryRowContext(context.Background(),
		`SELECT COALESCE(ended_reason, '') FROM sessions WHERE id = ?`, sessionID).Scan(&reason))
	return reason
}

// R6C-4: a reviewer turn refused because another process holds the session
// lock (a web tab's human turn, `sessions inject`) is not a failed review: the
// run keeps its last completed answer, exits clean, says on stderr that the
// pass did not run, and the refused (mutation-free) turn never writes
// ended_reason. Before the fix the refusal became the run's answer (empty
// final_text, exit_reason "error", exit 1) and ended_reason was written
// "error".
//
// Revert-check: (1) dropping the refusal case from scopeClosed restores the
// error envelope and the replaced answer; (2) dropping the loopTurn guard from
// ExecuteRun's ended_reason defer (R8A-1) writes the first turn's "end_turn"
// and the refused turn's reason to the log.
func TestRunLoop_ReviewerRefusedByLockKeepsAnswer(t *testing.T) {
	for name, mode := range map[string]RunMode{"json": RunModeJSON, "terse": RunModeTerse} {
		t.Run(name, func(t *testing.T) {
			rh := newReviewerHarness(t, false)
			writes := logEndedReasonWrites(t, rh.app)
			var stderr syncBuffer
			cliLoopStderr = &stderr
			t.Cleanup(func() { cliLoopStderr = nil })
			var endedAfterFirst string
			rh.afterFirstTurn(func() {
				endedAfterFirst = endedReasonOf(t, rh.app, rh.sessionID)
				foreign, err := session.TryAcquireSessionLock(rh.dataDir, rh.sessionID)
				require.NoError(t, err)
				t.Cleanup(func() { _ = foreign.Release() })
			})
			var out syncBuffer

			res, err := rh.app.RunNonInteractiveWithResult(loopCtx(t), &out, "do it",
				RunOverrides{ModelRole: config.SelectedModelTypeSmart, Origin: message.OriginCLI}, true, mode, rh.sessionID, false)

			require.NoError(t, err, "a refused review is not a failed run")
			require.NotNil(t, res)
			require.Equal(t, r3FirstAnswer, res.FinalText, "the last completed answer stays the run's answer")
			require.Equal(t, "end_turn", res.ExitReason)
			require.Empty(t, res.Error)
			require.Empty(t, rh.reviewerBodies(), "the refused reviewer turn reaches no provider")
			require.EqualValues(t, 1, rh.requests.Load())
			if mode == RunModeTerse {
				require.Equal(t, r3FirstAnswer, strings.TrimSpace(out.String()), "terse output is the kept answer")
			} else {
				require.Contains(t, out.String(), r3FirstAnswer)
				require.Equal(t, 1, strings.Count(out.String(), `"final_text"`))
			}
			require.Contains(t, stderr.String(), "reviewer pass", "the skipped pass is visible on stderr")
			require.Empty(t, endedAfterFirst, "the first loop turn leaves the row empty")
			require.Equal(t, res.ExitReason, endedReasonOf(t, rh.app, rh.sessionID), "the loop's exit writes the kept answer's reason")
			require.Equal(t, []string{"", res.ExitReason}, writes(), "neither turn writes ended_reason; the exit writes it once")
		})
	}
}

// r6HeldFirstJobHarness answers the first turn with an async bash job that
// never finishes (the gate file is never written) and, once it sees the job
// started, ends the turn: the loop then waits on the job.
func r6HeldFirstJobHarness(t *testing.T) *loopHarness {
	t.Helper()
	gate := t.TempDir() + "/gate"
	return newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, body []byte, _ bool, _ int) {
		_, _, lastTool := lastTurnParts(body)
		if strings.Contains(lastTool, "Async bash job") {
			loopText(w, "wait", "R6-WAITING", 30, 6)
			return
		}
		command := `until [ -f "` + gate + `" ]; do :; done`
		admissionWriteSSE(w, []string{
			admissionSSEToolCall("rb", "call-bash", "bash", `{"command":`+jsonString(command)+`,"description":"hold"}`),
			admissionSSEStop("rb", "tool_calls"),
		})
	})
}

// R6C-6: `rush run --timeout` (and Ctrl-C) while the loop waits on a job ends
// the run "canceled", but ExecuteRun's own write had set the row to the first
// turn's "end_turn", so `sessions show` said "Ended: end_turn". The loop's exit
// now writes the envelope's reason once, on a context detached from the
// cancelled run ctx.
//
// Revert-check: dropping the persist call from flushed (exitCanceled) leaves
// the row empty (since R8A-1 the turns write nothing themselves).
func TestRunLoop_TimeoutWhileWaitingWritesEnvelopeReason(t *testing.T) {
	h := r6HeldFirstJobHarness(t)
	writes := logEndedReasonWrites(t, h.app)
	ctx, cancel := context.WithCancel(loopCtx(t))
	stderr := &cancelOnWrite{substr: "still has open work", cancel: cancel}
	cliLoopStderr = stderr
	t.Cleanup(func() { cliLoopStderr = nil })

	res, out, err := h.run(ctx, RunOverrides{})

	require.ErrorIs(t, err, context.Canceled)
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Contains(t, out, `"exit_reason":"canceled"`)
	require.Equal(t, res.ExitReason, endedReasonOf(t, h.app, h.sessionID), "the row carries the envelope's exit reason")
	require.Equal(t, []string{"", "canceled"}, writes(), "the start clear and the loop's one exit write: loop turns write none")
}

// scopeOverrideSource answers CLIScope from a script once armed (after the
// first turn); everything else, and the answers before it, are the real ones.
type scopeOverrideSource struct {
	agent.ReactionDebtSource
	armed atomic.Bool
	state agent.CLIScopeState
	err   error
}

func (s *scopeOverrideSource) CLIScope(ctx context.Context, sessionID string) (agent.CLIScopeState, error) {
	if !s.armed.Load() {
		return s.ReactionDebtSource.CLIScope(ctx, sessionID)
	}
	return s.state, s.err
}

// R6C-6: the other between-turns exits (stuck debt, an operator cancel caught
// by the precheck, a persistently unreadable DB) each write the reason the
// envelope carries; the row never keeps the first turn's "end_turn".
//
// Revert-check: dropping the persist call from exit / flushed leaves
// the row empty for the matching case (since R8A-1 the turns write nothing).
func TestRunLoop_BetweenTurnsExitsWriteEnvelopeReason(t *testing.T) {
	origLimit, origPause := cliDBErrorRetryOverallLimit, cliDBRetryPause
	cliDBErrorRetryOverallLimit, cliDBRetryPause = 30*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { cliDBErrorRetryOverallLimit, cliDBRetryPause = origLimit, origPause })

	cases := []struct {
		name       string
		state      agent.CLIScopeState
		scopeErr   error
		cancelReq  bool
		wantReason string
	}{
		{name: "stuck", state: agent.CLIScopeState{Drain: agent.DrainStuck, Reason: "pull keeps failing"}, wantReason: "error"},
		{name: "operator cancel at the precheck", state: agent.CLIScopeState{Drain: agent.DrainOwed}, cancelReq: true, wantReason: "canceled"},
		{name: "persistent database error", scopeErr: errors.New("database is locked"), wantReason: "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
				loopText(w, "a", "R6-FIRST", 11, 3)
			})
			src := &scopeOverrideSource{ReactionDebtSource: driverSource(t, h.app), state: tc.state, err: tc.scopeErr}
			h.afterFirstTurn(func() {
				if tc.cancelReq {
					require.NoError(t, h.app.Sessions.RequestCancel(context.Background(), h.sessionID))
				}
				src.armed.Store(true)
			})
			var out, stderr syncBuffer
			overrides := RunOverrides{Origin: message.OriginCLI}
			turnOverrides := overrides
			l := &cliLoop{
				app: h.app, source: src, ctx: loopCtx(t), output: &out, mode: RunModeJSON, hideSpinner: true,
				overrides: overrides, turnOverrides: turnOverrides, prompt: "do it",
				continueSessionID: h.sessionID, started: time.Now(), sessionID: h.sessionID,
				lastBuffered: &bytes.Buffer{}, stderr: &stderr,
			}

			res, err := l.run()

			require.Error(t, err)
			require.NotNil(t, res)
			require.Equal(t, tc.wantReason, res.ExitReason)
			require.Equal(t, res.ExitReason, endedReasonOf(t, h.app, h.sessionID), "the row carries the envelope's exit reason")
		})
	}
}
