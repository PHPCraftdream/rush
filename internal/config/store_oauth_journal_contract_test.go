package config

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func requireOAuthLogsSanitized(t *testing.T, logs *codexRefreshLogs, secret string) {
	t.Helper()
	logs.mu.Lock()
	defer logs.mu.Unlock()
	for _, record := range logs.records {
		require.NotContains(t, record.Message, secret)
		record.Attrs(func(attr slog.Attr) bool {
			require.NotContains(t, attr.Value.String(), secret)
			return true
		})
	}
}

// Revert-check: journal retries retain published memory and never log injected secrets.
func TestOAuthJournal_TransientWriteFailuresAreDurableAndSanitized(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	logs := captureCodexRefreshLogs(t)
	secret := "fake-refresh-secret-not-for-logs"
	f.next.RefreshToken = secret
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	entered, release := pauseCodexPendingPersist(t)
	t.Cleanup(release)
	oldWrite, oldWait := oauthJournalAtomicWrite, oauthJournalRetryWait
	attempts := 0
	oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		attempts++
		pc, ok := f.store.Config().Providers.Get(f.used.ID)
		require.True(t, ok)
		require.Equal(t, f.next, pc.OAuthToken)
		if attempts <= 2 {
			return errors.New(secret)
		}
		return atomicWriteFile(path, data, mode)
	}
	oauthJournalRetryWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	t.Cleanup(func() { oauthJournalAtomicWrite, oauthJournalRetryWait = oldWrite, oldWait })
	joinOAuthJournalTestStoresBeforeRestore(t)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	require.Equal(t, 3, attempts)
	requireOAuthJournalToken(t, f)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(oauthJournalPath(f.path, f.used.ID))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	requireOAuthLogsSanitized(t, logs, secret)
}

// Revert-check: config retry errors log once, not once per attempt or with secrets.
func TestOAuthJournal_ConfigRetryLoggingIsRateLimited(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	logs := captureCodexRefreshLogs(t)
	secret := "fake-config-error-refresh-secret"
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	hookOAuthJournalFastRetry(t)
	entered := make(chan struct{})
	resume := make(chan struct{})
	var once sync.Once
	hookCodexPendingPersist(t, func(ctx context.Context, attempt int) error {
		if attempt < 10 {
			return errors.New(secret)
		}
		once.Do(func() { close(entered) })
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	t.Cleanup(func() { close(resume) })
	idle := hookOAuthJournalIdle(t, f.store)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	logs.mu.Lock()
	count := 0
	for _, record := range logs.records {
		if record.Message == "Failed to persist OAuth config; retrying" {
			count++
		}
	}
	logs.mu.Unlock()
	require.Equal(t, 1, count)
	requireOAuthLogsSanitized(t, logs, secret)
	resume <- struct{}{}
	waitOAuthJournalIdle(t, idle)
}

// Revert-check: recovery must hold the matching refresh sidecar across inspection.
func TestOAuthJournal_RecoveryHoldsRefreshLock(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	writeOAuthJournalFixture(t, f)
	old := oauthJournalRecoveryHook
	var checked atomic.Int32
	oauthJournalRecoveryHook = func() {
		lock, err := filelock.TryAcquireFileLock(oauthRefreshLockPath(f.path, f.used.ID))
		if lock != nil {
			_ = lock.Release()
		}
		var contended *filelock.ErrLockContended
		require.ErrorAs(t, err, &contended)
		checked.Add(1)
	}
	t.Cleanup(func() { oauthJournalRecoveryHook = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
	before := oauthJournalBeforeWorker
	oauthJournalBeforeWorker = func(ctx context.Context) { <-ctx.Done() }
	t.Cleanup(func() { oauthJournalBeforeWorker = before })
	joinOAuthJournalTestStoresBeforeRestore(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		t.Error("unexpected exchange")
		return nil, nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	f.store.stopOAuthJournalWorkers()
	require.Equal(t, int32(1), checked.Load())
}

// Revert-check: equal config bytes are not permission to delete an uncertain WAL.
func TestOAuthJournal_EqualTokenWaitsForDurability(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	writeCodexPendingDisk(t, f, f.next)
	writeOAuthJournalFixture(t, f)
	configTestHooks.Lock()
	old := configTestHooks.syncParent
	configTestHooks.syncParent = func(string) error { return errors.New("injected parent uncertainty") }
	configTestHooks.Unlock()
	t.Cleanup(func() { configTestHooks.Lock(); configTestHooks.syncParent = old; configTestHooks.Unlock() })
	joinOAuthJournalTestStoresBeforeRestore(t)
	_, record, err := recoverOAuthJournalFixture(t, f)
	require.Error(t, err)
	require.NotNil(t, record)
	requireOAuthJournalToken(t, f)
	configTestHooks.Lock()
	configTestHooks.syncParent = old
	configTestHooks.Unlock()
	_, record, err = recoverOAuthJournalFixture(t, f)
	require.NoError(t, err)
	require.Nil(t, record)
	_, err = os.Stat(oauthJournalPath(f.path, f.used.ID))
	require.True(t, os.IsNotExist(err))
}

// Revert-check: failed quarantine preservation still permits a fresh durable WAL.
func TestOAuthJournal_QuarantineAllPreservationFailsStillRefreshes(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	logs := captureCodexRefreshLogs(t)
	secret := "fake-corrupt-refresh-secret"
	path := oauthJournalPath(f.path, f.used.ID)
	require.NoError(t, os.WriteFile(path, []byte(secret), 0o600))
	oldRename, oldCopy := oauthJournalQuarantineRename, oauthJournalQuarantineCopy
	oauthJournalQuarantineRename = func(string, string) error { return errors.New(secret) }
	oauthJournalQuarantineCopy = func(string, string) error { return errors.New(secret) }
	t.Cleanup(func() { oauthJournalQuarantineRename, oauthJournalQuarantineCopy = oldRename, oldCopy })
	joinOAuthJournalTestStoresBeforeRestore(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	entered, release := pauseCodexPendingPersist(t)
	t.Cleanup(release)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	requireOAuthJournalToken(t, f)
	requireOAuthLogsSanitized(t, logs, secret)
}

// Revert-check: quarantine pruning must not delete active or other-provider files.
func TestOAuthJournal_QuarantinePruningIsProviderScoped(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	path := oauthJournalPath(f.path, f.used.ID)
	other := oauthJournalPath(f.path, "other-provider") + ".corrupt-1"
	active := []byte("active-placeholder")
	require.NoError(t, os.WriteFile(other, active, 0o600))
	for i := 0; i < 7; i++ {
		require.NoError(t, os.WriteFile(path, []byte("corrupt"), 0o600))
		quarantineOAuthJournal(path)
	}
	require.NoError(t, os.WriteFile(path, active, 0o600))
	pruneOAuthJournalQuarantines(path)
	for _, kept := range []string{path, other} {
		data, err := os.ReadFile(kept)
		require.NoError(t, err)
		require.Equal(t, active, data)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	count := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), filepath.Base(path)+".corrupt-") {
			count++
		}
	}
	require.Equal(t, 5, count)
}
