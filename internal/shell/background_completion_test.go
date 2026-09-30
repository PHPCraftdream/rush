// The completion hold (background_completion.go): a job whose OnDone callback
// is registered and has neither recorded its outcome nor returned still counts
// as pending, after its process exited and independently of the job table.
package shell

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// holdFixture is a manager with one running `sleep 30` owned by "s1".
func holdFixture(t *testing.T) (*BackgroundShellManager, *BackgroundShell) {
	t.Helper()
	m := newBackgroundShellManager()
	t.Cleanup(func() { m.Close(t.Context()) })
	bs, err := m.StartOwned(t.Context(), "s1", t.TempDir(), nil, "sleep 30", "")
	require.NoError(t, err)
	return m, bs
}

func eventuallyZero(t *testing.T, m *BackgroundShellManager, session, why string) {
	t.Helper()
	require.Eventually(t, func() bool { return m.PendingCompletionsOwned(session) == 0 }, 5*time.Second, 5*time.Millisecond, why)
}

// TestCompletionHold_HeldFromRegistrationUntilTheCallbackReturns: the hold
// starts at OnDone, survives the process exit and the shell's removal from the
// job table (Kill takes it out), and ends only when the callback returns.
//
// Revert-check: dropping takeCompletionHold from OnDone reports 0 at once.
func TestCompletionHold_HeldFromRegistrationUntilTheCallbackReturns(t *testing.T) {
	t.Parallel()
	m, bs := holdFixture(t)
	enter, inFn, leave := make(chan struct{}), make(chan struct{}), make(chan struct{})
	bs.OnDone(func() {
		<-enter
		close(inFn)
		<-leave
	})
	require.Equal(t, 1, m.PendingCompletionsOwned("s1"), "held while the shell still runs")
	require.Zero(t, m.PendingCompletionsOwned("other"), "another session's count is untouched")

	require.NoError(t, m.Kill(t.Context(), bs.ID))
	require.Zero(t, m.ActiveOwned("s1"), "the process exited and the job left the table")
	require.Equal(t, 1, m.PendingCompletionsOwned("s1"), "the callback has not entered: still pending")

	close(enter)
	<-inFn
	require.Equal(t, 1, m.PendingCompletionsOwned("s1"), "the callback is running: still pending")

	close(leave)
	eventuallyZero(t, m, "s1", "the callback returned: the hold is released")
}

// TestCompletionHold_PanickingCallbackReleases: a callback that panics (the
// shell recovers it) must not hold its session's scope open forever.
//
// Revert-check: releasing the hold outside the callback's deferred completion
// (only on a normal return) leaves it held after the panic.
func TestCompletionHold_PanickingCallbackReleases(t *testing.T) {
	t.Parallel()
	m, bs := holdFixture(t)
	bs.OnDone(func() { panic("callback failed") })
	require.NoError(t, m.Kill(t.Context(), bs.ID))
	eventuallyZero(t, m, "s1", "a panicking callback releases the hold")
}

// TestCompletionHold_RecordedEndsTheHoldBeforeTheCallbackReturns: the callback
// ends the hold once its outcome is durable, so its own re-checks (still inside
// the callback) see the session without the hold.
//
// Revert-check: making MarkCompletionRecorded a no-op keeps the count at 1
// while the callback is still running.
func TestCompletionHold_RecordedEndsTheHoldBeforeTheCallbackReturns(t *testing.T) {
	t.Parallel()
	m, bs := holdFixture(t)
	marked, leave := make(chan struct{}), make(chan struct{})
	seen := make(chan int, 1)
	bs.OnDone(func() {
		bs.MarkCompletionRecorded()
		bs.MarkCompletionRecorded() // idempotent
		seen <- m.PendingCompletionsOwned("s1")
		close(marked)
		<-leave
	})
	require.NoError(t, m.Kill(t.Context(), bs.ID))
	<-marked
	require.Equal(t, 0, <-seen, "inside the callback, after MarkCompletionRecorded")
	require.Zero(t, m.PendingCompletionsOwned("s1"))
	close(leave)
}

// TestCompletionHold_FinishedShellCountsForAtMostTheBound: a callback that never
// returns cannot hold a finished shell's scope open beyond completionHoldMax.
//
// Revert-check: dropping the bound keeps the stale hold counted forever.
func TestCompletionHold_FinishedShellCountsForAtMostTheBound(t *testing.T) {
	t.Parallel()
	m, bs := holdFixture(t)
	leave := make(chan struct{})
	t.Cleanup(func() { close(leave) })
	bs.OnDone(func() { <-leave })
	require.NoError(t, m.Kill(t.Context(), bs.ID))
	require.Equal(t, 1, m.PendingCompletionsOwned("s1"), "a fresh finish counts")

	bs.completedAt.Store(time.Now().Add(-completionHoldMax - time.Minute).Unix())
	require.Zero(t, m.PendingCompletionsOwned("s1"), "past the bound the wedged callback no longer holds the scope")
	require.Zero(t, m.PendingCompletionsOwned("s1"), "and the entry is dropped")
}

// TestCompletionHold_OnlyRegisteredShellsAndLastCallbackCount: a shell with no
// callback holds nothing; with two callbacks the hold lasts until the LAST one
// returns.
//
// Revert-check: releasing on any callback's return (not the last) reports 0
// while the second callback is still blocked.
func TestCompletionHold_OnlyRegisteredShellsAndLastCallbackCount(t *testing.T) {
	t.Parallel()
	m, bs := holdFixture(t)
	require.Zero(t, m.PendingCompletionsOwned("s1"), "no callback registered: nothing is owed")

	firstDone, leave := make(chan struct{}), make(chan struct{})
	bs.OnDone(func() { close(firstDone) })
	bs.OnDone(func() { <-leave })
	require.NoError(t, m.Kill(t.Context(), bs.ID))
	<-firstDone
	// Let the first callback's completion bookkeeping run.
	require.Eventually(t, func() bool { return bs.onDoneCount.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, 1, m.PendingCompletionsOwned("s1"), "the second callback is still owed")

	close(leave)
	eventuallyZero(t, m, "s1", "the last callback returned")
}

// TestCompletionHold_HandBuiltShellIsHarmless: a shell without a manager
// (tests build them by hand) registers callbacks as before.
func TestCompletionHold_HandBuiltShellIsHarmless(t *testing.T) {
	t.Parallel()
	bs := &BackgroundShell{done: make(chan struct{})}
	fired := make(chan struct{})
	bs.OnDone(func() { close(fired) })
	bs.MarkCompletionRecorded()
	close(bs.done)
	<-fired
}

// TestCompletionHold_ShutdownReleasesEveryHold: a manager closed at shutdown
// kills its shells; every callback runs, returns and releases, so no session's
// scope stays held.
//
// Revert-check: a hold released only by MarkCompletionRecorded (not by the
// callback's return) stays counted after Close for a callback that never
// records.
func TestCompletionHold_ShutdownReleasesEveryHold(t *testing.T) {
	t.Parallel()
	m, bs := holdFixture(t)
	other, err := m.StartOwned(t.Context(), "s2", t.TempDir(), nil, "sleep 30", "")
	require.NoError(t, err)
	bs.OnDone(func() {})
	other.OnDone(func() {})
	require.Equal(t, 1, m.PendingCompletionsOwned("s1"))
	require.Equal(t, 1, m.PendingCompletionsOwned("s2"))

	m.Close(t.Context())

	eventuallyZero(t, m, "s1", "shutdown released s1")
	eventuallyZero(t, m, "s2", "shutdown released s2")
}
