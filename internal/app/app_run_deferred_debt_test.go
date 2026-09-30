package app

// The `rush run` loop and the session policy must agree on "a reaction turn is
// owed": debt the policy will never allow a turn for (a bg-shell-done notice
// with AutoResumeOnJobDone off) used to keep nextStep reporting a
// turn owed, every Drain was refused as a no-turn, and the loop re-checked
// every 5s until --timeout/Ctrl-C. These tests drive the real
// RunNonInteractiveWithResult path against a real App and provider.

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// bgShellNoticeProvider answers every request with text; the FIRST one also
// inserts a bg-shell-done notice (the shell finished while the turn ran, after
// its last step-boundary pull), so the notice is still pending when the turn
// ends.
func bgShellNoticeProvider(appRef *atomic.Pointer[App], sessionID *atomic.Value, requests *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		n := requests.Add(1)
		if n == 1 {
			if a := appRef.Load(); a != nil {
				_ = a.asyncJobStore.InsertSessionNotice(context.Background(), sessionID.Load().(string),
					session.NoticeKindBGShellDone, "background shell finished: exit 0", true, "")
			}
		}
		text := "first answer"
		if n > 1 {
			text = "reacted to the shell"
		}
		admissionWriteSSE(w, []string{admissionSSEText("t", text), admissionSSEStop("t", "stop")})
	}
}

type bgShellRun struct {
	res       *RunResult
	err       error
	ctxErr    error
	requests  int32
	app       *App
	sessionID string
}

func runWithBGShellNotice(t *testing.T, autoResume bool, timeout time.Duration) bgShellRun {
	t.Helper()
	var appRef atomic.Pointer[App]
	var sid atomic.Value
	sid.Store("")
	var reqs atomic.Int32
	a, id := newAdmissionRaceApp(t, bgShellNoticeProvider(&appRef, &sid, &reqs))
	sid.Store(id)
	appRef.Store(a)
	if autoResume {
		opts := a.config.Config().Options
		if opts == nil {
			opts = &config.Options{}
			a.config.Config().Options = opts
		}
		on := true
		opts.AutoResumeOnJobDone = &on
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := a.RunNonInteractiveWithResult(ctx, io.Discard, "hello", RunOverrides{Origin: message.OriginCLI},
		true, RunModeJSON, id, false)
	return bgShellRun{res: res, err: err, ctxErr: ctx.Err(), requests: reqs.Load(), app: a, sessionID: id}
}

func bgShellNotices(t *testing.T, r bgShellRun) []db.SessionNotice {
	t.Helper()
	notices, err := db.New(r.app.DB()).ListSessionNoticesForOwner(context.Background(), r.sessionID)
	require.NoError(t, err)
	return notices
}

// TestRunNonInteractive_RefusedBGShellDebtEndsTheLoop: the only debt is a
// bg-shell-done notice and AutoResumeOnJobDone is off, so the policy refuses a
// reaction turn. The loop must end on its own -- no reaction turn, exit reason
// unaffected, the notice left for the next human turn -- instead of spinning
// until the deadline.
//
// Revert-check performed: put nextStep back on the old pair
// (pending-inclusive debt => turn owed, then ScopeOpen) -- the run kept
// looping, every Drain a no-turn, until the 25s context expired: err was
// non-nil, ctx.Err() DeadlineExceeded, ExitReason "canceled".
func TestRunNonInteractive_RefusedBGShellDebtEndsTheLoop(t *testing.T) {
	r := runWithBGShellNotice(t, false, 25*time.Second)

	require.NoError(t, r.ctxErr, "the loop must end on its own, not by the deadline")
	require.NoError(t, r.err)
	require.NotNil(t, r.res)
	require.NotEqual(t, "canceled", r.res.ExitReason)
	require.EqualValues(t, 1, r.requests, "no reaction turn may run for a refused bg-shell-only notice")
	require.Contains(t, r.res.FinalText, "first answer")

	notices := bgShellNotices(t, r)
	require.Len(t, notices, 1)
	require.EqualValues(t, 0, notices[0].Reacted, "the notice is not settled: it waits for the next turn")
	require.Equal(t, "pending", notices[0].Delivery, "no turn pulled it into history")
}

// TestRunNonInteractive_AutoResumeOnBGShellDebtStillReacts is the allowed
// direction of the same predicate: with AutoResumeOnJobDone on, the policy
// allows the turn, so the loop runs the reaction turn (and the previous test's
// silence is not a vacuous "the notice never counted as debt").
func TestRunNonInteractive_AutoResumeOnBGShellDebtStillReacts(t *testing.T) {
	r := runWithBGShellNotice(t, true, 25*time.Second)

	require.NoError(t, r.ctxErr)
	require.NoError(t, r.err)
	require.NotNil(t, r.res)
	require.EqualValues(t, 2, r.requests, "the allowed notice must get its reaction turn")
	require.Contains(t, r.res.FinalText, "reacted to the shell")

	notices := bgShellNotices(t, r)
	require.Len(t, notices, 1)
	require.EqualValues(t, 1, notices[0].Reacted)
}
