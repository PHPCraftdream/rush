package agent

// The CLI-scope completion hold, batch edition. A background shell that left
// its sync tool call is closed by notifyBackgroundJobDone (the OnDone
// notifier coordinator_tools.go wires), which persists the bg_shell_done wake
// notice behind the process-wide bgArrival gate and releases the shell
// manager's completion hold only after the insert commits. In the window
// "shell finished, hold not yet released" CLIScope must report WorkOpen (the
// hold read at coordinator_reaction_source.go), or the `rush run` loop exits
// before the notice becomes debt (R7B-1). The deleted legacy variant of the
// second test (row-observer-only, notify off -> terminal row, no wake notice)
// is an obsolete contract: the batch world always inserts the wake notice, so
// a finished shell always leaves wake debt behind.

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestCLIScope_ParkedNotifierKeepsTheScopeOpen freezes the notifier at the
// bgArrivalEnteredSeam while the shell is already finished: in exactly that
// window CLIScope must report WorkOpen, or the `rush run` loop exits before
// the bg_shell_done notice becomes debt.
//
// REVERT CHECK: dropping the completion-hold read from CLIScope leaves
// WorkOpen false here -- the shell is finished and the notice not yet
// inserted.
func TestCLIScope_ParkedNotifierKeepsTheScopeOpen(t *testing.T) {
	// bgArrivalEnteredSeam is a package-level atomic pointer: no t.Parallel,
	// and the hook is restored via t.Cleanup.
	ctx := context.Background()
	coord, conn, mgr, dir := b1BGShellFixture(t)
	notices := session.NewSessionNoticeStore(conn)
	sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, bgHoldCommand(bgGateName), "window")
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	seam := func(id string) {
		if id != "session" {
			return
		}
		close(entered) // sticky: every Eventually poll sees the notifier parked
		<-release      // parked inside persistBGShellCompletion, before bgArrival
	}
	bgArrivalEnteredSeam.Store(&seam)
	t.Cleanup(func() { bgArrivalEnteredSeam.Store(nil) })

	sh.OnDone(func() { coord.notifyBackgroundJobDone("session", sh) })

	// The shell ends NATURALLY (the gate file appears): a manager Kill takes
	// the shell out of the manager and absence-means-cancelled
	// (bgShellRowCancelled) would swallow the wake notice — cancelled shells
	// never wake. A naturally finished shell leaves ActiveOwned the moment
	// its process exits while staying in the manager's table, so the window
	// asserted below is unchanged.
	releaseBGShellGate(t, dir)
	require.True(t, sh.WaitContext(ctx))

	// The shell is finished and the notifier parked at the seam, so the
	// notice is not inserted yet.
	require.Eventually(t, func() bool {
		select {
		case <-entered:
		default:
			return false
		}
		rows, err := notices.ListSessionNotices(ctx, "session")
		return err == nil && len(rows) == 0
	}, 10*time.Second, 10*time.Millisecond, "the parked notifier has not inserted the wake notice yet")
	require.Zero(t, mgr.ActiveOwned("session"), "the shell itself is finished: this is the window")
	require.Positive(t, mgr.PendingCompletionsOwned("session"), "the completion hold is open")

	state, err := coord.CLIScope(ctx, "session")
	require.NoError(t, err)
	require.True(t, state.WorkOpen, "the completion hold is the open work in this window")

	close(release) // the notice commits; the notifier records it and returns
	require.Eventually(t, func() bool {
		return mgr.PendingCompletionsOwned("session") == 0
	}, 10*time.Second, 10*time.Millisecond, "the hold ends only after the notice insert commits")

	// The notice is now debt (pending-inclusive). This bare fixture runs with
	// autonomy off (nil cfg) and persistentMode false, so the arbiter defers
	// the bg-shell-only debt instead of owing a turn; DrainOwed is therefore
	// NOT asserted, but the debt must be visible and nothing may keep the
	// scope open any more.
	state, err = coord.CLIScope(ctx, "session")
	require.NoError(t, err)
	require.False(t, state.WorkOpen, "the hold is gone: nothing keeps the scope open")
	require.NotEqual(t, DrainNone, state.Drain, "the committed notice is visible debt")
}
