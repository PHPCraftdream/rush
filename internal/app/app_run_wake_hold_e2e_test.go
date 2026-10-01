// Stage 5a end to end through the real `rush run` loop: a ONE-SHOT wake
// schedule (wakein) holds the run open until it fires, the wake_fired
// notice becomes an Owed Drain and the run ends with the reaction's answer;
// --timeout/`sessions cancel` cut the wait short as "canceled"; a `loop`
// schedule never holds the run -- the scope close cancels it (one stderr
// line, one envelope warning), alone or beside a running job.
//
// Revert-checks: dropping OnceWakeOpen from CLIScope makes (a) exit before
// the fire (final text and request-count assertions go red) and (b)/(д)
// never wait (the canceled assertions go red); dropping
// cancelLoopSchedulesAtClose leaves the loop schedule active -- the
// cancelled-row and warning assertions of (в)/(г) go red. The hard ctx
// deadline in every scenario makes a regression fail, never hang forever.
package app

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func wakeScheduleState(t *testing.T, application *App, id string) string {
	t.Helper()
	row, err := db.New(application.DB()).GetWakeSchedule(context.Background(), id)
	require.NoError(t, err)
	return row.State
}

// wakeScheduleID returns the session's first wake_schedules row id (each
// scenario here creates exactly one).
func wakeScheduleID(t *testing.T, application *App, sessionID string) string {
	t.Helper()
	ctx := context.Background()
	rows, err := application.WakeScheduleStore().ListSchedules(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the scenario creates exactly one schedule")
	return rows[0].ID
}

// wakeE2E is the shared scenario runner: firstTurn lists the tool calls of
// the first turn (a wakein/loop schedule, optionally plus other calls);
// turns after the scripted first two are produced by extra (nil = refuse:
// the provider answers 400 so a rogue turn fails the run instead of hanging).
func wakeE2E(
	t *testing.T,
	firstTurn []string,
	deadline time.Duration,
	extra func(n int) []string,
	// release, when non-nil, runs in a goroutine once the loop is
	// provably waiting on open work (the heartbeat) -- the way the held
	// job of scenario (г) is released DURING the run.
	release func(),
) (*App, string, *RunResult, *chainStderr, int32, error) {
	t.Helper()
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()

	var requestsC atomic.Int32
	var stderrBuf chainStderr
	restore := cliLoopStderr
	cliLoopStderr = &stderrBuf
	t.Cleanup(func() { cliLoopStderr = restore })

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch n := requestsC.Add(1); n {
		case 1:
			calls := append([]string{}, firstTurn...)
			calls = append(calls, admissionSSEStop("turn", "tool_calls"))
			admissionWriteSSE(w, calls)
		case 2:
			admissionWriteSSE(w, []string{admissionSSEText("a", "scheduled"), admissionSSEStop("a", "stop")})
		default:
			chunks := extra(int(requestsC.Load()))
			if chunks == nil {
				http.Error(w, "unexpected model call: the loop ran a turn it must not", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, chunks)
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), deadline)
	defer cancel()
	if release != nil {
		go func() {
			require.Eventually(t, func() bool {
				return strings.Contains(stderrBuf.String(), "still has open work")
			}, 60*time.Second, 20*time.Millisecond, "the wait heartbeat")
			release()
		}()
	}
	var stdout strings.Builder
	res, err := application.RunNonInteractiveWithResult(ctx, &stdout, "wake timers",
		RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	return application, sessionID, res, &stderrBuf, requestsC.Load(), err
}

// (a) A once wakein holds the loop: the run waits for the fire, the
// wake_fired notice drives exactly one Drain whose answer is the run's.
func TestRunNonInteractive_OnceWakeHoldsLoopAndReacts(t *testing.T) {
	application, sessionID, res, stderr, requests, runErr := wakeE2E(t,
		[]string{admissionSSEToolCall("t", "call-wakein", "wakein", `{"delay_seconds":5,"message":"timer done"}`)},
		90*time.Second,
		func(n int) []string {
			if n == 3 {
				return []string{admissionSSEText("w", "woke up"), admissionSSEStop("w", "stop")}
			}
			return nil
		}, nil)
	require.NoError(t, runErr)

	require.EqualValues(t, 3, requests, "first turn + the wake_fired drain")
	require.Contains(t, stderr.String(), "still has open work")
	require.Contains(t, stderr.String(), "wake schedule wake_")
	require.Equal(t, "woke up", res.FinalText, "the wake reaction is the run's answer")
	require.Equal(t, "end_turn", res.ExitReason)
	require.Equal(t, "done", wakeScheduleState(t, application, wakeScheduleID(t, application, sessionID)))
}

// (b) --timeout while waiting on a far-future wakein: the run ends
// "canceled" with an envelope, never waiting out the timer.
func TestRunNonInteractive_TimeoutDuringWakeWaitIsCanceled(t *testing.T) {
	_, _, res, _, _, runErr := wakeE2E(t,
		[]string{admissionSSEToolCall("t", "call-wakein", "wakein", `{"delay_seconds":3600,"message":"later"}`)},
		3*time.Second,
		func(int) []string { return nil }, nil)
	require.ErrorIs(t, runErr, context.DeadlineExceeded)
	require.Equal(t, "canceled", res.ExitReason)
	require.NotEmpty(t, res.SessionID, "the cancel still flushes a real envelope")
}

// (в) A loop schedule with no other open work: the run exits WITHOUT
// waiting, the schedule row is cancelled, and one stderr line and one
// envelope warning say so.
func TestRunNonInteractive_LoopScheduleCancelledAtRunEnd(t *testing.T) {
	application, sessionID, res, stderr, requests, runErr := wakeE2E(t,
		[]string{admissionSSEToolCall("t", "call-loop", "loop", `{"every_seconds":300,"message":"tick"}`)},
		30*time.Second,
		func(int) []string { return nil }, nil)
	require.NoError(t, runErr)

	require.EqualValues(t, 2, requests, "first turn only; the loop never held the run")
	require.Equal(t, "scheduled", res.FinalText)
	require.Equal(t, 1, strings.Count(stderr.String(), "loop schedule(s) cancelled at run end"))
	require.Contains(t, stderr.String(), "rush run: session")
	var warn string
	for _, w := range res.Warnings {
		if strings.Contains(w, "loop schedule(s) cancelled") {
			warn = w
		}
	}
	require.Equal(t, "1 loop schedule(s) cancelled at run end", warn, "the close line is an envelope warning")
	id := wakeScheduleID(t, application, sessionID)
	require.Equal(t, "cancelled", wakeScheduleState(t, application, id))
}

// (г) A loop schedule PLUS a held background job: the loop waits on the
// job, the job's completion is drained, and the close then cancels the
// loop. The far-future wakein here doubles as the "loop removed, once
// kept" contrast: the close must not touch it.
func TestRunNonInteractive_LoopBesideRunningJobCancelledAtClose(t *testing.T) {
	donePath := strings.ReplaceAll(filepath.Join(t.TempDir(), "w-done"), "\\", "/")
	application, sessionID, res, stderr, requests, runErr := wakeE2E(t,
		[]string{
			admissionSSEToolCall("t", "call-loop", "loop", `{"every_seconds":300,"message":"tick"}`),
			sseToolCallIndex("b", "call-held", "bash",
				`{"command":"until [ -f `+donePath+` ]; do sleep 5; done; echo W-DONE","description":"held job"}`, 1),
		},
		90*time.Second,
		func(n int) []string {
			if n == 3 {
				return []string{admissionSSEText("f", "after job"), admissionSSEStop("f", "stop")}
			}
			return nil
		},
		func() {
			require.NoError(t, os.WriteFile(donePath, []byte("go"), 0o644))
		})
	require.NoError(t, runErr)

	require.EqualValues(t, 3, requests, "first turn + the job-completion drain")
	require.Equal(t, "after job", res.FinalText)
	require.Equal(t, 1, strings.Count(stderr.String(), "loop schedule(s) cancelled at run end"))
	// The once kind is untouched by the close; here none exists, so the
	// loop row is the cancelled one.
	rows, err := application.WakeScheduleStore().ListSchedules(context.Background(), sessionID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "cancelled", rows[0].State)
}

// (д) `sessions cancel` while the run waits on a far-future wakein: the run
// ends "canceled" with an envelope, within the 5s wait fallback.
func TestRunNonInteractive_SessionsCancelDuringWakeWait(t *testing.T) {
	var stderrBuf chainStderr
	restore := cliLoopStderr
	cliLoopStderr = &stderrBuf
	t.Cleanup(func() { cliLoopStderr = restore })
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()

	var requestsC atomic.Int32
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch n := requestsC.Add(1); n {
		case 1:
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("t", "call-wakein", "wakein", `{"delay_seconds":3600,"message":"later"}`),
				admissionSSEStop("turn", "tool_calls"),
			})
		case 2:
			admissionWriteSSE(w, []string{admissionSSEText("a", "scheduled"), admissionSSEStop("a", "stop")})
		default:
			http.Error(w, "unexpected model call: the cancel must end the wait", http.StatusBadRequest)
		}
	})

	done := make(chan error, 1)
	var res *RunResult
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		var stdout strings.Builder
		r, err := application.RunNonInteractiveWithResult(ctx, &stdout, "wake timers",
			RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
		res = r
		done <- err
	}()

	var canceled atomic.Bool
	require.Eventually(t, func() bool {
		if !strings.Contains(stderrBuf.String(), "still has open work") {
			return false
		}
		if canceled.CompareAndSwap(false, true) {
			require.NoError(t, application.Sessions.RequestCancel(context.Background(), sessionID))
		}
		return canceled.Load()
	}, 20*time.Second, 20*time.Millisecond, "landing the cancel at the heartbeat")
	// The run's own 5s wait fallback re-reads the flag even with no hint;
	// the cancel surfaces as the operator's cancel error, not a clean end.
	err := <-done
	require.ErrorContains(t, err, "cancelled by user")
	require.NotNil(t, res)
	require.Equal(t, "canceled", res.ExitReason)
	require.Equal(t, 2, int(requestsC.Load()), "no turn after the cancel")
}
