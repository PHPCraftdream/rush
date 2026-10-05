// The CLI-scope completion hold: a background shell that left its sync tool
// call is closed by two OnDone callbacks on their own goroutines -- the row
// observer (bgshell_claim.go) commits the terminal async_jobs row, and
// notifyBackgroundJobDone persists the bg_shell_done wake notice behind the
// process-wide bgArrival gate. In the window "(a) committed, (b) not yet"
// CLIScope used to see no running row and no debt and closed the scope, so
// the shell's result was never reacted to in that run. The fix folds the
// shell manager's completion hold (taken at OnDone registration, released
// only after the notice insert commits) into WorkOpen, the same way
// childScopeDrained already gates a delegation's release on it (R7B-1).
package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestCLIScope_ParkedNotifierKeepsTheScopeOpen freezes the notifier at the
// bgArrivalEnteredSeam after the observer has already committed the shell's
// terminal row: in exactly that window CLIScope must report WorkOpen, or the
// `rush run` loop exits before the bg_shell_done notice becomes debt.
//
// REVERT CHECK: dropping the completion-hold read from CLIScope leaves
// WorkOpen false here -- this test FAILED before the fix (WorkOpen: false,
// want true) while the row was already terminal and the notice not yet
// inserted.
func TestCLIScope_ParkedNotifierKeepsTheScopeOpen(t *testing.T) {
	// bgArrivalEnteredSeam is a package-level atomic pointer: no t.Parallel,
	// and the hook is restored via t.Cleanup.
	ctx := context.Background()
	coord, store, mgr, dir := newBGShellFixture(t)
	sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "sleep 30", "window")
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
	bgShellEscape(t, coord, sh.ID) // registers the row observer

	require.NoError(t, mgr.Kill(ctx, sh.ID))
	sh.Wait()

	// (a) happened, (b) did not: the row is terminal while the notifier is
	// parked at the seam, so the notice is not inserted yet.
	require.Eventually(t, func() bool {
		select {
		case <-entered:
		default:
			return false
		}
		row, err := store.Get(ctx, "session", sh.ID)
		return err == nil && (row.State == "completed" || row.State == "failed")
	}, 10*time.Second, 10*time.Millisecond, "the observer must commit the row while the notifier is parked")
	notices, err := store.ListSessionNotices(ctx, "session")
	require.NoError(t, err)
	require.Empty(t, notices, "the parked notifier has not inserted the wake notice yet")
	require.Zero(t, mgr.ActiveOwned("session"), "the shell itself is finished: this is the window")

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

// TestCLIScope_NoNotifierScopeClosesWithTheRow: with the row observer as the
// ONLY callback (notify off), the completion hold ends when that callback
// returns, and the finished shell keeps nothing open. The hold must not keep
// a scope open longer than its callbacks.
func TestCLIScope_NoNotifierScopeClosesWithTheRow(t *testing.T) {
	t.Parallel()
	coord, store, mgr, dir := newBGShellFixture(t)
	ctx := context.Background()
	sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "echo done", "no notifier")
	require.NoError(t, err)

	bgShellEscape(t, coord, sh.ID) // the observer is the only OnDone
	sh.Wait()

	require.Eventually(t, func() bool {
		row, err := store.Get(ctx, "session", sh.ID)
		return err == nil && (row.State == "completed" || row.State == "failed") &&
			mgr.PendingCompletionsOwned("session") == 0
	}, 10*time.Second, 10*time.Millisecond, "the observer committed the row and returned")

	state, err := coord.CLIScope(ctx, "session")
	require.NoError(t, err)
	require.False(t, state.WorkOpen)
	require.Equal(t, DrainNone, state.Drain)
}
