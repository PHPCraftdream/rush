package app

import (
	"context"
	"net/http"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// R5C-5: LiveSessionDrivers reads every marker in one statement and keeps those
// whose host is not provably dead -- this process's own host and a sibling
// App's included -- keyed by session id, with the driver's PID.
//
// Revert-check: keeping dead hosts' markers fails the dead-host assertion;
// dropping the own-host markers (ForeignLiveDriver's rule) fails the first.
func TestLiveSessionDrivers_OwnSiblingAndDeadHosts(t *testing.T) {
	h := newLoopHarness(t, func(_ *loopHarness, w http.ResponseWriter, _ []byte, _ bool, _ int) {
		loopText(w, "a", "x", 1, 1)
	})
	ctx := context.Background()
	mk := func(title string) string {
		sess, err := h.app.Sessions.Create(ctx, title)
		require.NoError(t, err)
		return sess.ID
	}
	own, sibling, dead, none := mk("own"), mk("sibling"), mk("dead"), mk("none")

	require.NoError(t, h.app.asyncJobStore.ClaimSessionDriver(ctx, own))
	siblingStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 5151, "sibling")
	t.Cleanup(func() { _ = siblingStore.Close(context.Background()) })
	require.NoError(t, siblingStore.ClaimSessionDriver(ctx, sibling))
	deadStore := session.NewAsyncJobStore(h.app.DB(), h.dataDir, 4242, "dead")
	require.NoError(t, deadStore.ClaimSessionDriver(ctx, dead))
	require.NoError(t, deadStore.SimulateCrashForTest())

	got, err := h.app.LiveSessionDrivers(ctx)

	require.NoError(t, err)
	require.Contains(t, got, own, "this process's own loop is a live driver")
	require.Contains(t, got, sibling)
	require.EqualValues(t, 5151, got[sibling].PID)
	require.Equal(t, session.HostStatusAlive, got[sibling].Status)
	require.NotContains(t, got, dead, "a crashed host's marker keeps nothing open")
	require.NotContains(t, got, none)
	require.Len(t, got, 2)
}

// An App with no store or no database handle answers an empty set, never an
// error: read-only status commands run on such Apps.
func TestLiveSessionDrivers_NoStoreIsEmpty(t *testing.T) {
	got, err := (&App{}).LiveSessionDrivers(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
}
