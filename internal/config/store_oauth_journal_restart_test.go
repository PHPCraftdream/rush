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

// Revert-check: a stopped never-run persister leaves WAL recoverable without exchange.
func TestOAuthJournal_ConfigCrashRestartSkipsExchange(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	old := oauthJournalBeforeWorker
	entered := make(chan struct{}, 1)
	oauthJournalBeforeWorker = func(ctx context.Context) { entered <- struct{}{}; <-ctx.Done() }
	t.Cleanup(func() { oauthJournalBeforeWorker = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	f.store.stopOAuthJournalWorkers()
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	requireOAuthJournalToken(t, f)
	second := newTestConfigStore(testStoreOpts{config: f.store.Config(), globalDataPath: f.path})
	second.workingDir = f.store.workingDir
	second.SetProviderRuntimeConfig(f.used.ID, f.used)
	trackOAuthJournalTestStore(t, second)
	calls := 0
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { calls++; return nil, nil })
	require.NoError(t, second.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	second.stopOAuthJournalWorkers()
	require.Zero(t, calls)
	g := f
	g.store = second
	assertCodexRefreshTokens(t, g, f.next, f.used.OAuthToken)
}

// Revert-check: failed Load preserves exact WAL bytes until repaired Load recovers.
func TestOAuthJournal_FailedLoadPreservesJournalThenRepair(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	writeOAuthJournalFixture(t, f)
	journalPath := oauthJournalPath(f.path, f.used.ID)
	before, err := os.ReadFile(journalPath)
	require.NoError(t, err)
	config, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.path, []byte("{invalid config"), 0o600))
	failed, err := Load(f.store.workingDir, "", false)
	require.Error(t, err)
	require.Nil(t, failed)
	after, err := os.ReadFile(journalPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, os.WriteFile(f.path, config, 0o600))
	loaded, err := Load(f.store.workingDir, "", false)
	require.NoError(t, err)
	trackOAuthJournalTestStore(t, loaded)
	loaded.stopOAuthJournalWorkers()
	pc, ok := loaded.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	require.Equal(t, f.next, pc.OAuthToken)
}

// Revert-check: quarantine rename fallback preserves corrupt bytes without secret logs.
func TestOAuthJournal_QuarantineRenameFallbackRefreshes(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	logs := captureCodexRefreshLogs(t)
	secret := "fake-corrupt-token-for-log-oracle"
	corrupt := []byte("invalid:" + secret)
	path := oauthJournalPath(f.path, f.used.ID)
	require.NoError(t, os.WriteFile(path, corrupt, 0o600))
	old := oauthJournalQuarantineRename
	oauthJournalQuarantineRename = func(string, string) error { return errors.New(secret) }
	t.Cleanup(func() { oauthJournalQuarantineRename = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
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
	requireOAuthLogsSanitized(t, logs, secret)
}

// Revert-check: uncertain equal bytes must retain B protection against a stale reload.
func TestOAuthJournal_UncertainEqualReloadRetainsPending(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	idle := hookOAuthJournalIdle(t, f.store)
	entered, release := pauseCodexPendingPersist(t)
	t.Cleanup(release)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	configTestHooks.Lock()
	old := configTestHooks.syncParent
	configTestHooks.syncParent = func(string) error { return errors.New("uncertain directory durability") }
	configTestHooks.Unlock()
	t.Cleanup(func() { configTestHooks.Lock(); configTestHooks.syncParent = old; configTestHooks.Unlock() })
	joinOAuthJournalTestStoresBeforeRestore(t)
	writeCodexPendingDisk(t, f, f.next)
	require.NoError(t, f.store.ReloadFromDisk(context.Background()))
	_, record, err := recoverOAuthJournalFixture(t, f)
	require.Error(t, err)
	require.NotNil(t, record)
	assertCodexPending(t, f, true)
	requireOAuthJournalToken(t, f)
	writeCodexPendingDisk(t, f, f.used.OAuthToken)
	require.NoError(t, f.store.ReloadFromDisk(context.Background()))
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	assertCodexPending(t, f, true)
	configTestHooks.Lock()
	configTestHooks.syncParent = old
	configTestHooks.Unlock()
	release()
	waitOAuthJournalIdle(t, idle)
	assertCodexPending(t, f, false)
	assertCodexRefreshTokens(t, f, f.next, f.next)
}
