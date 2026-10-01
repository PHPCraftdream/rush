// The reaction chain guard (#1113), end to end through the real `rush run`
// loop (design test 6): the first turn launches a HELD background job W plus a
// pure wait command (`echo tick`); every chain Drain the model makes is only
// another `echo tickN`. The guard must stop the chain after exactly 3 Drain
// turns -- one stderr line, one envelope warning -- while the loop keeps
// waiting on W; releasing W produces exactly one more Drain (a real fact) and
// the run ends with the last turn's answer and exit reason. Without W the loop
// exits after the 3 Drains with the debt still deferred.
//
// Revert-check: disabling the drainPolicy chain branch makes the provider take
// a 4th chain request -- the handler answers 400 ("unexpected model call"),
// the run fails and the request-count assertion goes red. The hard ctx
// deadline plus the 400 guarantee the test fails rather than hangs.
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
// background job that blocks until the done-file appears; the file is written
// once the loop is provably inside its "waiting on open work" state (the
// heartbeat line), so W can never complete before the guard's deferral check.
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
	// W is held until the loop is provably waiting on open work; then the
	// done-file appears and W's completion becomes the next real fact.
	var stderrBuf chainStderr
	restore := cliLoopStderr
	cliLoopStderr = &stderrBuf
	t.Cleanup(func() { cliLoopStderr = restore })

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch n := requestsC.Add(1); {
		case n == 1:
			calls := []string{}
			if withW {
				calls = append(calls, sseToolCallIndex("w", "call-w", "bash",
					`{"command":"until [ -f `+donePath+` ]; do sleep 5; done; echo W-DONE","description":"held job"}`, 1))
			}
			calls = append(calls, admissionSSEToolCall("t1", "call-tick1", "bash", `{"command":"echo tick1","description":"tick"}`))
			calls = append(calls, admissionSSEStop("turn", "tool_calls"))
			admissionWriteSSE(w, calls)
		case n == 2:
			admissionWriteSSE(w, []string{admissionSSEText("a", "first answer"), admissionSSEStop("a", "stop")})
		case n >= 3 && n <= 8:
			// Each chain Drain is two provider steps: one bash `echo tickN`
			// tool call, then a plain text step that ends the leg (the model
			// has nothing to say while waiting). n=3/4 -> tick2, 5/6 -> tick3,
			// 7/8 -> tick4.
			step := n - 2
			if step%2 == 1 {
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("t", "call-tick"+itoa(int(step)/2+2), "bash", `{"command":"echo tick`+itoa(int(step/2+2))+`","description":"tick"}`),
					admissionSSEStop("t", "tool_calls"),
				})
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("w", "waiting "+itoa(int(step/2))), admissionSSEStop("w", "stop")})
		case withW && n == 9:
			admissionWriteSSE(w, []string{admissionSSEText("f", "final answer"), admissionSSEStop("f", "stop")})
		default:
			http.Error(w, "unexpected model call (n="+itoa(int(n))+"): the chain guard let another drain through", http.StatusBadRequest)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if withW {
		// Release W only after drain3's accounting is done (tick3's attempt
		// counter is the marker): the loop's deferral check follows within
		// milliseconds, while W's 30s poll makes an earlier completion
		// practically impossible -- the race the guard's outcome depends on
		// must not be left to chance.
		// tick4's own job must be terminal too: under load its `echo` can
		// finish after W, and W's Drain then pulls W alone, leaving tick4 to
		// the guard after the run's answer.
		go func() {
			for i := 0; i < 3000; i++ {
				tick3, err3 := application.asyncJobStore.Get(ctx, sessionID, "call-tick3")
				tick4, err4 := application.asyncJobStore.Get(ctx, sessionID, "call-tick4")
				if err3 == nil && tick3.Reacted >= 1 && tick3.Delivery == "done" &&
					err4 == nil && tick4.State != "running" {
					releaseW()
					return
				}
				time.Sleep(20 * time.Millisecond)
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

	require.EqualValues(t, 9, requests, "first turn (2 steps) + 3 chain drains (2 steps each) + the W-fact drain")
	require.Equal(t, 1, strings.Count(stderr.String(), "reaction chain stopped"), stderr.String())
	require.Len(t, chainWarnings(res), 1)
	require.Contains(t, stderr.String(), chainWarnings(res)[0])
	require.Equal(t, "final answer", res.FinalText, "the W completion's drain is the answer")
	require.Equal(t, "end_turn", res.ExitReason, "the guard never rewrites the exit reason")
	// The W drain reacts W AND the guard-deferred tick4 (the policy allows the
	// turn: W is a row outside the chain's set).
	require.Eventually(t, func() bool {
		open, err := app.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
		return err == nil && !open
	}, 10*time.Second, 10*time.Millisecond, "the W drain reacted every row")
}

func TestRunNonInteractive_ChainGuardExitsWithoutWork(t *testing.T) {
	app, sessionID, res, stderr, requests := chainE2E(t, false)

	require.EqualValues(t, 8, requests, "first turn (2 steps) + 3 chain drains (2 steps each), then the loop exits")
	require.Equal(t, 1, strings.Count(stderr.String(), "reaction chain stopped"), stderr.String())
	require.Len(t, chainWarnings(res), 1)
	require.Equal(t, "end_turn", res.ExitReason, "the exit reason is the last drain's own, untouched")
	require.Equal(t, "waiting 3", res.FinalText, "the last completed drain's text is the answer")
	open, err := app.asyncJobStore.ReactionDebtExists(context.Background(), sessionID)
	require.NoError(t, err)
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
