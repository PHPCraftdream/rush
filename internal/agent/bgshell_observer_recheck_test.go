package agent

// Batch translation (H4 lane P5) of the deleted claim-time bg-shell observer
// tests (bgshell_claim.go's async_jobs row machinery is gone: the batch
// escape is claimed and settled by the batch ingress, and the ONLY completion
// callback is the OnDone notifier coordinator_tools.go wires to
// notifyBackgroundJobDone). What survives is the completion-hold contract:
// persistBGShellCompletion takes the hold (BackgroundShellManager) only for
// its own duration, and while the notifier is parked at the
// bgArrivalEnteredSeam the hold must stay with it -- a callback that released
// the hold before the notice insert commits would let CLIScope close the
// scope in the row-committed, notice-not-yet window (R7B-1).
//
// The former TestAsyncTool_BGShellObserverReleasesParkedDelegationWithoutNotifier
// is an obsolete contract: with the claim-time observer deleted,
// NotifyOnBackgroundJobDone no longer selects between two callbacks -- the
// notifier is always installed, so a "no completion callback" scenario cannot
// exist. The notifier-driven release of a parked delegation is covered by the
// recheck/delegation release tests (recheckChild's batch successor and
// coordinator_recheck_child_test.go).

import (
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
	"github.com/stretchr/testify/require"
)

// newBGShellCompletionFixture builds a coordinator whose bg-shell completion
// plumbing is fully wired: background manager, notice store, and node facts
// on a fresh one-migration store.
func newBGShellCompletionFixture(t *testing.T) (*coordinator, *session.SessionNoticeStore, *shell.BackgroundShellManager, string) {
	t.Helper()
	conn, dataDir := batchFixtureConn(t)
	// The shells are owned by the literal id "session"; its bg_shell_done
	// notices FK-anchor on sessions (id).
	ensureFixtureSession(t, conn, "session")
	mgr := shell.NewBackgroundShellManager()
	t.Cleanup(func() { mgr.KillAll(t.Context()) })
	coord := newBatchRuntimeCoordinator()
	coord.noticeStore = session.NewSessionNoticeStore(conn)
	coord.background = mgr
	bindBatchNodeFacts(t, coord, conn, dataDir)
	return coord, coord.noticeStore, mgr, t.TempDir()
}

// With notifications ON the notifier owns the notice, and a recheck that ran
// before it is saved would release the delegation too early (R7B-1): the
// completion hold has to stay until the notifier returns. "On" is the default
// (no config, no options, an unset pointer) as well as an explicit true --
// notifyOnBackgroundJobDone gates only the immediate Drain, never the hold.
//
// REVERT CHECK: releasing the hold in persistBGShellCompletion regardless of
// the notification setting makes PendingCompletionsOwned drop to 0 while the
// notifier is still parked; a helper that read the default as "off" does the
// same for the config cases.
func TestBGShellCompletion_KeepsTheHoldWhileTheNotifierIsPending(t *testing.T) {
	// bgArrivalEnteredSeam is a package-level pointer: no t.Parallel.
	cases := []struct {
		name string
		opts *config.Options // nil: no config store at all
	}{
		{name: "no config", opts: nil},
		{name: "unset option", opts: &config.Options{}},
		{name: "explicit true", opts: &config.Options{NotifyOnBackgroundJobDone: boolPtr(true)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			coord, notices, mgr, dir := newBGShellCompletionFixture(t)
			if tc.opts != nil {
				coord.cfg = config.NewLibraryStore(&config.Config{Options: tc.opts}, t.TempDir())
			}
			sh, err := mgr.StartOwned(t.Context(), "session", dir, nil, "echo hold", "notifier pending")
			require.NoError(t, err)

			entered := make(chan struct{})
			release := make(chan struct{})
			seam := func(id string) {
				if id != "session" {
					return
				}
				close(entered)
				<-release
			}
			bgArrivalEnteredSeam.Store(&seam)
			t.Cleanup(func() { bgArrivalEnteredSeam.Store(nil) })
			sh.OnDone(func() { coord.notifyBackgroundJobDone("session", sh) })

			sh.Wait()

			require.Eventually(t, func() bool {
				select {
				case <-entered:
				default:
					return false
				}
				rows, err := notices.ListSessionNotices(t.Context(), "session")
				return err == nil && len(rows) == 0
			}, 10*time.Second, 10*time.Millisecond, "the notifier is parked before the wake notice is inserted")
			require.Positive(t, mgr.PendingCompletionsOwned("session"),
				"the hold must stay with the pending notifier")

			close(release)
			require.Eventually(t, func() bool { return mgr.PendingCompletionsOwned("session") == 0 },
				10*time.Second, 10*time.Millisecond, "the hold ends once the notifier returned")
		})
	}
}
