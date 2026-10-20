// The reaction chain guard (#1113), end to end through the real `rush run`
// loop (design test 6): the first turn launches a HELD background job W plus
// two short timers, and the model reacts to each timer's completion with a
// Drain leg that polls for W with a pure wait command (`echo tick-N`). While
// W is the session's own undelivered work the in-turn progress guard (§2.2 of
// docs/plans/2026-10-01-in-turn-progress-guard.md) refuses every one of those
// launches at once -- no workLedger.Start, no durable row in either
// authority, no #1113 claim
// -- so a leg that carries neither a claim nor progress is invisible to
// chainLink: no reaction chain forms, the "reaction chain stopped" line never
// appears, and the loop simply keeps waiting on W. Releasing W (once both
// legs are served) produces exactly one more Drain -- a real fact -- whose
// answer ends the run. The second test in this file keeps the old shape with
// no W at all: there the sleeps really run (ownWork is false and a leg's
// streak never reaches S), #1113 counts three links and stops the chain.
//
// The two timers are the legs' debt source: only a completed job is reaction
// debt, and a Drain the loop would never open cannot be refused. The old
// `sleep 1` tick of this scenario is gone -- the wait commands live in the
// legs now, where the guard refuses them. Under RUSH_BATCH_RUNTIME=1 the
// guard's own-work half is ledger-sourced and blind to batch work, so the
// legs are not refused; the strict no-root oracle records that gap (see
// .rush/stdin/jb-f6a3.findings.md, finding 4).
//
// Revert-check: dropping ownWork from refuseWait (turn_progress_guard.go)
// lets the echo legs through -- each starts an async job whose completion
// feeds the next leg, #1113 counts the chain and the line appears (and the
// request count changes), so this test goes red. The hard ctx deadline plus
// the provider's 400 guarantee the test fails rather than hangs.
package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

type chainStderr struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *chainStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *chainStderr) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// chainE2E runs one `rush run` scenario. withW: the first turn also launches a
// background job that blocks until the done-file appears and two timers whose
// completions open the poll legs; the file is written once the loop is
// provably past those legs (the provider served them), so W can never
// complete before the guard's own verdict on them. Without W the sleeps of
// the old #1113 scenario run for real and the chain forms.
func chainE2E(t *testing.T, withW bool) (app *App, sessionID string, res *RunResult, stderr *chainStderr, requests int32) {
	t.Helper()
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()

	var requestsC atomic.Int32
	var donePath string
	var releaseOnce sync.Once
	releaseW := func() {
		if donePath != "" {
			releaseOnce.Do(func() {
				require.NoError(t, os.WriteFile(donePath, []byte("go"), 0o644), "release W")
			})
		}
	}
	if withW {
		// Forward slashes: the path lands inside a JSON tool-call argument
		// and inside the POSIX shell command.
		donePath = strings.ReplaceAll(filepath.Join(t.TempDir(), "w-done"), "\\", "/")
	}
	// W is held until the loop is provably past the legs its guard refused;
	// then the done-file appears and W's completion becomes the next real fact.
	var stderrBuf chainStderr
	restore := cliLoopStderr
	cliLoopStderr = &stderrBuf
	t.Cleanup(func() { cliLoopStderr = restore })

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		n := int(requestsC.Add(1))
		if !withW {
			// No W: every sleep launches for real (ownWork is false and a
			// leg's streak never reaches S), so each leg's completion feeds the
			// next one and #1113 counts the chain.
			// The ticks must detach in BOTH modes (legacy honors the explicit
			// flag; batch escapes after the 3s window): a sync batch job would
			// complete inside the window and never become reaction debt. The 4s
			// commands are forced by the fixed 3s window
			// (inlineWindowSeconds, batch_install_tools.go:271).
			switch {
			case n == 1:
				calls := []string{admissionSSEToolCall("t1", "call-tick1", "bash", `{"command":"sleep 4","run_in_background":true,"description":"tick"}`)}
				calls = append(calls, admissionSSEStop("turn", "tool_calls"))
				admissionWriteSSE(w, calls)
			case n == 2:
				admissionWriteSSE(w, []string{admissionSSEText("a", "first answer"), admissionSSEStop("a", "stop")})
			case n >= 3 && n <= 8:
				// Each chain Drain is two provider steps: one detached bash `sleep 4`
				// tool call, then a plain text step that ends the leg (the model
				// has nothing to say while waiting). n=3/4 -> tick2, 5/6 -> tick3,
				// 7/8 -> tick4.
				step := n - 2
				if step%2 == 1 {
					admissionWriteSSE(w, []string{
						admissionSSEToolCall("t", "call-tick"+itoa(int(step)/2+2), "bash", `{"command":"sleep 4","run_in_background":true,"description":"tick"}`),
						admissionSSEStop("t", "tool_calls"),
					})
					return
				}
				admissionWriteSSE(w, []string{admissionSSEText("w", "waiting "+itoa(int(step/2))), admissionSSEStop("w", "stop")})
			default:
				http.Error(w, "unexpected model call (n="+itoa(n)+"): the chain guard let another drain through", http.StatusBadRequest)
			}
			return
		}
		// W alive: the polls are pure wait commands, each carrying the tool's
		// required `description` -- fantasy validates a call against the tool's
		// schema BEFORE any wrapper sees it (agent.go's validateToolCall), and a
		// call it marks Invalid never reaches the guard at all: its plain error
		// result then classifies as act, the streak never grows and the guard
		// would never refuse anything (the hang T5 hit first).
		// The timers must detach in BOTH modes: under RUSH_BATCH_RUNTIME=1 a
		// bash call without run_in_background is a sync batch job that
		// completes inside the 3s inline window and never becomes reaction
		// debt, so their completions could never open the poll legs (legacy
		// honors the explicit flag; batch escapes after the 3s window). The 4s
		// commands are forced by the fixed 3s window (inlineWindowSeconds,
		// batch_install_tools.go:271).
		switch n {
		case 1:
			calls := []string{
				sseToolCallIndex("w", "call-w", "bash",
					`{"command":"until [ -f `+donePath+` ]; do sleep 5; done; echo W-DONE","run_in_background":true,"description":"held job"}`, 1),
				sseToolCallIndex("t1", "call-timer1", "bash", `{"command":"sleep 4","run_in_background":true,"description":"timer"}`, 2),
				sseToolCallIndex("t2", "call-timer2", "bash", `{"command":"sleep 8","run_in_background":true,"description":"timer"}`, 3),
			}
			calls = append(calls, admissionSSEStop("turn", "tool_calls"))
			admissionWriteSSE(w, calls)
		case 2:
			admissionWriteSSE(w, []string{admissionSSEText("a", "first answer"), admissionSSEStop("a", "stop")})
		case 3, 5:
			tick := itoa((n - 1) / 2)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("t", "call-tick"+tick, "bash", `{"command":"echo tick-`+tick+`","description":"tick"}`),
				admissionSSEStop("t", "tool_calls"),
			})
		case 4, 6:
			admissionWriteSSE(w, []string{admissionSSEText("w", "waiting "+itoa((n-2)/2)), admissionSSEStop("w", "stop")})
		case 7:
			admissionWriteSSE(w, []string{admissionSSEText("f", "final answer"), admissionSSEStop("f", "stop")})
		default:
			http.Error(w, "unexpected model call (n="+itoa(n)+")", http.StatusBadRequest)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if withW {
		// Release W only once both poll legs are served: the guard has refused
		// their echoes by then (no job, no claim, so no chain either), and W's
		// 5s poll makes an earlier completion practically impossible. The
		// staggered timers keep the legs apart: the first leg reacts the first
		// timer alone, the second one the second timer.
		go func() {
			for i := 0; i < 6000; i++ {
				if requestsC.Load() >= 6 {
					releaseW()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	var stdout bytes.Buffer
	res, err := application.RunNonInteractiveWithResult(ctx, &stdout, "wait for the jobs",
		RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	return application, sessionID, res, &stderrBuf, requestsC.Load()
}

func TestRunNonInteractive_ChainGuardStopsAfterThreeDrainsWaitsForWork(t *testing.T) {
	app, sessionID, res, stderr, requests := chainE2E(t, true)

	require.EqualValues(t, 7, requests, "first turn (2 steps) + 2 refused poll legs (2 steps each) + the W-fact drain")
	require.NotContains(t, stderr.String(), "reaction chain stopped",
		"the refused waits start no jobs, so #1113 never sees a chain")
	require.Empty(t, chainWarnings(res), "no chain stopped warning in the envelope either")
	// The in-turn guard refuses each poll: no job is ever created -- flag
	// OFF: no async_jobs row; flag ON: no batch row in ANY state (terminal
	// included). Under RUSH_BATCH_RUNTIME=1 this currently FAILS BY
	// DESIGN OF THE ORACLE: the guard's own-work half reads only the
	// legacy in-memory ledger (agent_turn_step.go:320, work_ledger.go
	// running), which batch execution never populates, so the echo legs
	// execute and leave terminal roots. That gap is recorded as finding 4
	// in .rush/stdin/jb-f6a3.findings.md; the oracle stays strict.
	for _, callID := range []string{"call-tick1", "call-tick2"} {
		f6a3NoWorkForCall(t, app, sessionID, callID)
	}
	require.Equal(t, "final answer", res.FinalText, "the W completion's drain is the answer")
	require.Equal(t, "end_turn", res.ExitReason, "the guard never rewrites the exit reason")
	require.Eventually(t, func() bool {
		debt, ok := f6a3ReactionDebtExistsOK(t, app, sessionID)
		return ok && !debt
	}, 10*time.Second, 10*time.Millisecond, "the W drain reacted W")
}

func TestRunNonInteractive_ChainGuardExitsWithoutWork(t *testing.T) {
	app, sessionID, res, stderr, requests := chainE2E(t, false)

	require.EqualValues(t, 8, requests, "first turn (2 steps) + 3 chain drains (2 steps each), then the loop exits")
	require.Equal(t, 1, strings.Count(stderr.String(), "reaction chain stopped"), stderr.String())
	require.Len(t, chainWarnings(res), 1)
	require.Equal(t, "end_turn", res.ExitReason, "the exit reason is the last drain's own, untouched")
	require.Equal(t, "waiting 3", res.FinalText, "the last completed drain's text is the answer")
	open := f6a3ReactionDebtExists(t, app, sessionID)
	require.True(t, open, "the deferred debt stays for the next turn")
}

// chainWarnings filters the envelope's warnings down to the guard's own line:
// the runs' other diagnostics (bash tool notices etc.) are not this test's
// subject and vary between iterations.
func chainWarnings(res *RunResult) []string {
	var out []string
	for _, w := range res.Warnings {
		if strings.Contains(w, "reaction chain stopped") {
			out = append(out, w)
		}
	}
	return out
}

// sseToolCallIndex is admissionSSEToolCall with an explicit tool_calls index,
// so several calls can be streamed in one assistant message.
func sseToolCallIndex(id, callID, name, args string, index int) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, id, index, callID, name, args)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
