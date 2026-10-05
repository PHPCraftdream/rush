package agent

// B9-7: a natural exit racing job_kill must deliver EXACTLY ONE outcome:
// either the row is cancelled and the callbacks insert nothing, or the kill
// defers (AlreadyTerminal) and the natural completion inserts its one notice.

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// Order 1, the callback first: the notifier is parked at the arrival seam
// (before bgArrival) while the row is still running, then job_kill arrives.
// Under the gate the kill sees IsDone and defers; the released callback
// inserts its one notice. Without the gate the kill's cancelled transition
// raced the callback's insert, delivering BOTH "stopped" and "finished".
//
// REVERT CHECK: removing the IsDone defer from StopBackgroundShellRow makes the
// kill win the cancelled transition for an already-exited process (verdict
// Stopped, no AlreadyTerminal); removing the gate as well lets the callback
// insert its notice too -- the contradictory "stopped" + "finished" pair.
func TestStopBackgroundShellRow_NaturalExitFirstDeliversExactlyOneOutcome(t *testing.T) {
	// bgArrivalEnteredSeam is a package-level atomic pointer: no t.Parallel,
	// and the hook is restored via t.Cleanup.
	coord, store, mgr, dir := newBGShellFixture(t)
	coord.asyncJobs.coord = coord
	sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "sleep 2", "race")
	require.NoError(t, err)
	_, err = store.ClaimShell(context.Background(), "session", sh.ID, false)
	require.NoError(t, err)

	entered := make(chan struct{})
	release := make(chan struct{})
	seam := func(id string) {
		if id != "session" {
			return
		}
		close(entered) // sticky: the polls below see the notifier parked
		<-release      // parked inside persistBGShellCompletion, before bgArrival
	}
	bgArrivalEnteredSeam.Store(&seam)
	t.Cleanup(func() { bgArrivalEnteredSeam.Store(nil) })

	// The notifier registers only after the seam is armed, and the process
	// exits on its own a couple of seconds in, so the callback can only start
	// once the park is in place. It is the only OnDone (no observer): the row
	// stays running while it is parked, which is exactly the insert that used
	// to race the kill's cancelled transition.
	sh.OnDone(func() { coord.notifyBackgroundJobDone("session", sh) })
	sh.Wait()
	require.Eventually(t, func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}, 10*time.Second, 10*time.Millisecond, "the notifier must be parked at the seam")

	text, claimID, verdict := coord.asyncJobs.StopBackgroundShellRow("session", sh.ID)
	// The process had already exited when job_kill arrived: the kill defers to
	// the natural completion path instead of claiming a "stop" for it.
	require.Equal(t, tools.JobStopAlreadyTerminal, verdict, "unexpected verdict (%q)", text)
	require.Empty(t, claimID, "a deferred kill claims nothing")

	close(release) // the callback finishes either way
	require.Eventually(t, func() bool { return mgr.PendingCompletionsOwned("session") == 0 },
		10*time.Second, 10*time.Millisecond, "the released callback must finish")
	notices, err := store.ListSessionNotices(context.Background(), "session")
	require.NoError(t, err)

	// EXACTLY ONE delivery: the deferred kill answered with the real output and
	// the natural path inserted its one notice.
	require.Len(t, notices, 1, "exactly one bg_shell_done notice")
}

// Order 2, job_kill first: the row is cancelled while the process still runs;
// the callbacks that start after the exit find the cancelled row and stand
// down, so the stopped answer ships alone.
func TestStopBackgroundShellRow_KillFirstDeliversExactlyOneOutcome(t *testing.T) {
	t.Parallel()
	coord, store, shellID := newBGShellKillFixture(t)
	sh, ok := coord.background.GetOwned("session", shellID)
	require.True(t, ok)
	sh.OnDone(func() { coord.notifyBackgroundJobDone("session", sh) })

	text, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
	require.Equal(t, tools.JobStopStopped, verdict)
	require.NotEmpty(t, text)

	require.NoError(t, coord.background.KillOwned(shortKillCtx(t), "session", shellID))
	sh.Wait()
	require.Eventually(t, func() bool { return coord.background.PendingCompletionsOwned("session") == 0 },
		5*time.Second, 10*time.Millisecond)
	notices, err := store.ListSessionNotices(context.Background(), "session")
	require.NoError(t, err)
	require.Empty(t, notices, "a job_kill-stopped shell ships only the stopped answer")
}

// A completion callback that lost the race to job_kill (the row is already
// cancelled when it reaches the gate) inserts nothing and claims no slot; a
// shell nobody killed still gets its one notice (control).
//
// REVERT CHECK: dropping the cancelled check from persistBGShellCompletion
// inserts a bg_shell_done notice for the killed shell.
func TestPersistBGShellCompletion_CancelledRowInsertsNothing(t *testing.T) {
	t.Parallel()
	coord, store, shellID := newBGShellKillFixture(t)
	_, _, verdict := coord.asyncJobs.StopBackgroundShellRow("session", shellID)
	require.Equal(t, tools.JobStopStopped, verdict)

	claimed := coord.persistBGShellCompletion("session", shellID, "finished: exit 0")
	require.False(t, claimed, "a cancelled shell takes no auto-resume slot")
	notices, err := store.ListSessionNotices(context.Background(), "session")
	require.NoError(t, err)
	require.Empty(t, notices, "the callback that lost the race inserts nothing")

	// Control: the same call for a live, never-killed shell inserts its notice.
	coord2, store2, shellID2 := newBGShellKillFixture(t)
	coord2.persistBGShellCompletion("session", shellID2, "finished: exit 0")
	notices2, err := store2.ListSessionNotices(context.Background(), "session")
	require.NoError(t, err)
	require.Len(t, notices2, 1, "an un-killed shell's completion is delivered")
}
