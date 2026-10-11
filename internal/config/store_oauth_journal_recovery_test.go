package config

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func writeOAuthJournalFixture(t *testing.T, f codexRefreshFixture) {
	t.Helper()
	record := oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(f.used.OAuthToken), Token: f.next}
	data, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, atomicWriteFile(oauthJournalPath(f.path, f.used.ID), data, 0o600))
}

func recoverOAuthJournalFixture(t *testing.T, f codexRefreshFixture) (*oauth.Token, *oauthJournalRecord, error) {
	t.Helper()
	path := oauthRefreshLockPath(f.path, f.used.ID)
	lock, err := acquireConfigFileLock(context.Background(), path)
	require.NoError(t, err)
	defer releaseConfigFileLock(path, lock)
	return f.store.recoverCodexOAuthJournal(context.Background(), ScopeGlobal, f.used.ID)
}

// Revert-check: restart arbitration must respect predecessor, expiry ties, and logout.
func TestOAuthJournal_RecoveryRules(t *testing.T) {
	for _, name := range []string{"equal", "later", "tie", "earlier", "predecessor", "logout", "expired"} {
		t.Run(name, func(t *testing.T) {
			f := newCodexRefreshReloadFixture(t)
			disk := f.used.OAuthToken
			removed := false
			switch name {
			case "equal":
				disk, removed = f.next, true
			case "later", "tie", "earlier":
				c := *f.next
				c.AccessToken, c.RefreshToken = "external-access", "external-refresh"
				if name == "later" {
					c.ExpiresAt++
				}
				if name == "earlier" {
					c.ExpiresAt--
				}
				disk, removed = &c, name != "earlier"
			case "logout":
				disk, removed = nil, true
			case "expired":
				b := *f.next
				b.ExpiresAt = f.used.OAuthToken.ExpiresAt + 1
				f.next = &b
			}
			writeCodexPendingDisk(t, f, disk)
			require.NoError(t, f.store.ReloadFromDisk(context.Background()))
			writeOAuthJournalFixture(t, f)
			adopted, record, err := recoverOAuthJournalFixture(t, f)
			require.NoError(t, err)
			if removed {
				require.Nil(t, record)
				_, err := os.Stat(oauthJournalPath(f.path, f.used.ID))
				require.True(t, os.IsNotExist(err))
				if disk == nil {
					pc, ok := f.store.Config().Providers.Get(f.used.ID)
					require.True(t, !ok || pc.OAuthToken == nil)
				} else {
					require.Equal(t, disk, adopted)
				}
				return
			}
			require.Equal(t, f.next, adopted)
			require.NotNil(t, record)
			assertCodexRefreshTokens(t, f, f.next, disk)
			idle := hookOAuthJournalIdle(t, f.store)
			f.store.startOAuthJournalWorker(ScopeGlobal, f.used.ID)
			waitOAuthJournalIdle(t, idle)
			assertCodexRefreshTokens(t, f, f.next, f.next)
		})
	}
}

// Revert-check: a corrupt config cannot erase a rotation across store restart.
func TestOAuthJournal_CorruptConfigRestartThenRepair(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	original, err := os.ReadFile(f.path)
	require.NoError(t, err)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		require.NoError(t, os.WriteFile(f.path, []byte("{invalid config"), 0o600))
		return f.next, nil
	})
	oldBefore := oauthJournalBeforeWorker
	oauthJournalBeforeWorker = func(ctx context.Context) { <-ctx.Done() }
	t.Cleanup(func() { oauthJournalBeforeWorker = oldBefore })
	joinOAuthJournalTestStoresBeforeRestore(t)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	f.store.stopOAuthJournalWorkers()
	requireOAuthJournalToken(t, f)

	second := newTestConfigStore(testStoreOpts{config: f.store.Config(), globalDataPath: f.path})
	second.workingDir = f.store.workingDir
	second.SetProviderRuntimeConfig(f.used.ID, f.used)
	trackOAuthJournalTestStore(t, second)
	g := f
	g.store = second
	calls := 0
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { calls++; return nil, nil })
	require.NoError(t, second.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	require.Zero(t, calls)
	pc, ok := second.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	require.Equal(t, f.next, pc.OAuthToken)
	requireOAuthJournalToken(t, g)
	second.stopOAuthJournalWorkers()
	require.NoError(t, os.WriteFile(f.path, original, 0o600))

	third := newTestConfigStore(testStoreOpts{config: second.Config(), globalDataPath: f.path})
	third.workingDir = second.workingDir
	trackOAuthJournalTestStore(t, third)
	g.store = third
	oauthJournalBeforeWorker = nil
	idle := hookOAuthJournalIdle(t, third)
	_, record, err := recoverOAuthJournalFixture(t, g)
	require.NoError(t, err)
	require.NotNil(t, record)
	third.startOAuthJournalWorker(ScopeGlobal, f.used.ID)
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, g, f.next, f.next)
}

// Revert-check: successful Load must recover WAL before another exchange.
func TestOAuthJournal_LoadRecoversAfterRepair(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	writeOAuthJournalFixture(t, f)
	calls := 0
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { calls++; return nil, nil })
	loaded, err := Load(f.store.workingDir, "", false)
	require.NoError(t, err)
	trackOAuthJournalTestStore(t, loaded)
	loaded.stopOAuthJournalWorkers()
	pc, ok := loaded.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	require.Equal(t, f.next, pc.OAuthToken)
	require.Zero(t, calls)
}
