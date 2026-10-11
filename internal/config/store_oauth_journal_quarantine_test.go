package config

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// Revert-check: corrupt active journals must not block a fresh rotation.
func TestOAuthJournal_CorruptJournalQuarantinedBeforeRefresh(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	path := oauthJournalPath(f.path, f.used.ID)
	corrupt := []byte("{invalid journal")
	require.NoError(t, os.WriteFile(path, corrupt, 0o600))
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		_, err := os.Stat(path)
		require.True(t, os.IsNotExist(err))
		return f.next, nil
	})
	entered, release := pauseCodexPendingPersist(t)
	t.Cleanup(release)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	requireOAuthJournalToken(t, f)
	names, err := filepath.Glob(path + ".corrupt-*")
	require.NoError(t, err)
	require.Len(t, names, 1)
	preserved, err := os.ReadFile(names[0])
	require.NoError(t, err)
	require.Equal(t, corrupt, preserved)
}

// Revert-check: rename failure must preserve bytes and bound quarantine retention.
func TestOAuthJournal_QuarantineCopyFallbackAndCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rush.json.oauth-pending-test.json")
	old := oauthJournalQuarantineRename
	oauthJournalQuarantineRename = func(string, string) error { return errors.New("injected rename failure") }
	t.Cleanup(func() { oauthJournalQuarantineRename = old })
	for i := 0; i < 7; i++ {
		require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0o600))
		quarantineOAuthJournal(path)
	}
	names, err := filepath.Glob(path + ".corrupt-*")
	require.NoError(t, err)
	require.Len(t, names, 5)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
}

// Revert-check: earlier external credentials must not displace durable B.
func TestOAuthJournal_EarlierExternalTokenDoesNotDisplacePending(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	idle := hookOAuthJournalIdle(t, f.store)
	entered, release := pauseCodexPendingPersist(t)
	t.Cleanup(release)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	earlier := *f.next
	earlier.AccessToken, earlier.RefreshToken = "earlier-access", "earlier-refresh"
	earlier.ExpiresAt--
	writeCodexPendingDisk(t, f, &earlier)
	require.NoError(t, f.store.ReloadFromDisk(context.Background()))
	assertCodexPending(t, f, true)
	assertCodexRefreshTokens(t, f, f.next, &earlier)
	release()
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, f.next, f.next)
}
