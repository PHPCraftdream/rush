package config

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Revert-check: permanent failure releases callers before joining seam users.
func TestOAuthJournalOutbox_PermanentWriteFailureReturns(t *testing.T) {
	for _, name := range []string{"budget", "caller-canceled-after-exchange"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			logs := captureCodexRefreshLogs(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			exchanged := make(chan struct{})
			stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
				close(exchanged)
				if name == "caller-canceled-after-exchange" {
					cancel()
				}
				return f.next, nil
			})
			oldWrite, oldWait, oldBudget := oauthJournalAtomicWrite, oauthJournalRetryWait, oauthJournalWriteBudget
			oauthJournalWriteBudget = 10 * time.Millisecond
			var failing atomic.Bool
			failing.Store(true)
			failed := make(chan struct{})
			var once sync.Once
			journalPath := oauthJournalPath(f.path, f.used.ID)
			oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
				if path == journalPath && failing.Load() {
					once.Do(func() { close(failed) })
					return errors.New("injected permanent journal storage failure")
				}
				return oldWrite(path, data, mode)
			}
			oauthJournalRetryWait = func(ctx context.Context, _ time.Duration) error { return waitCodexRefresh(ctx, time.Millisecond) }
			result, done := make(chan error, 1), make(chan struct{})
			t.Cleanup(func() {
				failing.Store(false)
				<-done
				f.store.stopOAuthJournalWorkers()
				oauthJournalAtomicWrite, oauthJournalRetryWait, oauthJournalWriteBudget = oldWrite, oldWait, oldBudget
			})
			go func() {
				defer close(done)
				result <- f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, f.used, nil)
			}()
			awaitCodexSignal(t, exchanged)
			if name == "budget" {
				awaitCodexSignal(t, failed)
			}
			select {
			case err := <-result:
				assert.NoError(t, err)
			case <-time.After(100 * time.Millisecond):
				t.Error("refresh did not return nil within 100ms while journal writes permanently fail (write budget 10ms)")
			}
			logs.mu.Lock()
			errorsLogged, handoffs := 0, 0
			for _, record := range logs.records {
				if record.Level == slog.LevelError {
					errorsLogged++
				}
				if record.Message == "OAuth journal write deferred to background persistence" {
					handoffs++
					filename := ""
					record.Attrs(func(attr slog.Attr) bool {
						if attr.Key == "file" {
							filename = attr.Value.String()
						}
						return true
					})
					assert.Equal(t, filepath.Base(journalPath), filename)
				}
			}
			logs.mu.Unlock()
			assert.LessOrEqual(t, errorsLogged, 1, "synchronous failure and handoff must not emit separate Error lines")
			assert.Equal(t, 1, handoffs, "handoff must emit one stable Error message")
			pc, ok := f.store.Config().Providers.Get(f.used.ID)
			require.True(t, ok)
			assert.Equal(t, f.next, pc.OAuthToken)
			_, err := os.Stat(journalPath)
			assert.True(t, os.IsNotExist(err))
			entry := f.store.oauthOutboxCurrent(f.path, f.used.ID)
			require.NotNil(t, entry)
			assert.Equal(t, f.next, entry.record.Token)
			cancel()
			assert.Same(t, entry, f.store.oauthOutboxCurrent(f.path, f.used.ID))
			lock, lockErr := filelock.TryAcquireFileLock(oauthRefreshLockPath(f.path, f.used.ID))
			require.NoError(t, lockErr)
			require.NoError(t, releaseConfigFileLock(oauthRefreshLockPath(f.path, f.used.ID), lock))
			codexRefreshActive.Lock()
			_, active := codexRefreshActive.calls[codexRefreshKey{f.store, f.used.ID}]
			codexRefreshActive.Unlock()
			assert.False(t, active)
			require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
			require.NoError(t, f.store.ReloadFromDisk(context.Background()))
			pc, ok = f.store.Config().Providers.Get(f.used.ID)
			require.True(t, ok)
			assert.Equal(t, f.next, pc.OAuthToken)
		})
	}
}

// Revert-check: worker repair journals first and same-used B can rotate to C.
func TestOAuthJournalOutbox_WorkerRepairAndSameUsedRotation(t *testing.T) {
	f := newOAuthJournalRefreshRecoveryFixture(t)
	oldWrite, oldBudget := oauthJournalAtomicWrite, oauthJournalWriteBudget
	oauthJournalWriteBudget = time.Millisecond
	var failing atomic.Bool
	failing.Store(true)
	oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		if failing.Load() {
			return errors.New("injected storage failure")
		}
		return oldWrite(path, data, mode)
	}
	t.Cleanup(func() { oauthJournalAtomicWrite, oauthJournalWriteBudget = oldWrite, oldBudget })
	joinOAuthJournalTestStoresBeforeRestore(t)
	calls := 0
	c := *f.next
	c.AccessToken, c.RefreshToken, c.ExpiresAt = "fixture-C-access", "fixture-C-refresh", c.ExpiresAt+3600
	stubCodexRefresh(t, func(_ context.Context, _ *http.Client, refresh string) (*oauth.Token, error) {
		calls++
		if calls == 1 {
			assert.Equal(t, f.used.OAuthToken.RefreshToken, refresh)
			return f.next, nil
		}
		assert.Equal(t, f.next.RefreshToken, refresh)
		return &c, nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	assert.Equal(t, 1, calls)
	usedB := f.used
	usedB.OAuthToken = f.next
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, usedB, nil))
	assert.Equal(t, 2, calls)
	entry := f.store.oauthOutboxCurrent(f.path, f.used.ID)
	require.NotNil(t, entry)
	assert.Equal(t, &c, entry.record.Token)
	require.NoError(t, f.store.ReloadFromDisk(context.Background()))
	pc, ok := f.store.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	assert.Equal(t, &c, pc.OAuthToken)
	oldPersist := codexRefreshBeforePersist
	codexRefreshBeforePersist = func(context.Context, int) error {
		record, err := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
		assert.NoError(t, err)
		require.NotNil(t, record)
		assert.Equal(t, &c, record.Token)
		return nil
	}
	t.Cleanup(func() { codexRefreshBeforePersist = oldPersist })
	joinOAuthJournalTestStoresBeforeRestore(t)
	failing.Store(false)
	// The existing fixture worker stays blocked; direct attempts use real files.
	for attempt := range 3 {
		done, err := f.store.oauthJournalWorkerAttempt(context.Background(), ScopeGlobal, f.used.ID, attempt)
		require.NoError(t, err)
		if done {
			break
		}
	}
	assert.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
	assertCodexRefreshTokens(t, f, &c, &c)
	_, err := os.Stat(oauthJournalPath(f.path, f.used.ID))
	assert.True(t, os.IsNotExist(err))
}

// Revert-check: actual worker persists after the handed-off caller cancels.
func TestOAuthJournalOutbox_ActualWorkerRepair(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	oldWrite, oldBudget, oldWait := oauthJournalAtomicWrite, oauthJournalWriteBudget, oauthJournalWorkerWait
	oauthJournalWriteBudget = time.Millisecond
	var failing atomic.Bool
	failing.Store(true)
	oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		if failing.Load() {
			return errors.New("injected storage failure")
		}
		return oldWrite(path, data, mode)
	}
	waiting, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	oauthJournalWorkerWait = func(ctx context.Context, delay time.Duration) error {
		once.Do(func() { close(waiting) })
		select {
		case <-resume:
			return waitCodexRefresh(ctx, time.Millisecond)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() {
		oauthJournalAtomicWrite, oauthJournalWriteBudget, oauthJournalWorkerWait = oldWrite, oldBudget, oldWait
	})
	joinOAuthJournalTestStoresBeforeRestore(t)
	oldPersist := codexRefreshBeforePersist
	codexRefreshBeforePersist = func(context.Context, int) error {
		record, err := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
		assert.NoError(t, err)
		if assert.NotNil(t, record) {
			assert.Equal(t, f.next, record.Token)
		}
		return nil
	}
	t.Cleanup(func() { codexRefreshBeforePersist = oldPersist })
	joinOAuthJournalTestStoresBeforeRestore(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	idle := hookOAuthJournalIdle(t, f.store)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, f.used, nil))
	cancel()
	awaitCodexSignal(t, waiting)
	require.NotNil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
	failing.Store(false)
	close(resume)
	waitOAuthJournalIdle(t, idle)
	f.store.stopOAuthJournalWorkers()
	assert.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
	assertCodexRefreshTokens(t, f, f.next, f.next)
}

// Revert-check: reload supersession cannot resurrect an obsolete owned rotation.
func TestOAuthJournalOutbox_ExternalConfigArbitration(t *testing.T) {
	for _, name := range []string{"earlier", "later", "tie", "logout"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			oldWrite, oldBudget := oauthJournalAtomicWrite, oauthJournalWriteBudget
			oauthJournalWriteBudget = time.Millisecond
			var failing atomic.Bool
			failing.Store(true)
			oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
				if failing.Load() {
					return errors.New("injected storage failure")
				}
				return oldWrite(path, data, mode)
			}
			t.Cleanup(func() { oauthJournalAtomicWrite, oauthJournalWriteBudget = oldWrite, oldBudget })
			joinOAuthJournalTestStoresBeforeRestore(t)
			stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
			require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
			entry := f.store.oauthOutboxCurrent(f.path, f.used.ID)
			require.NotNil(t, entry)
			external := *f.next
			external.AccessToken, external.RefreshToken = "fixture-external-access", "fixture-external-refresh"
			disk := &external
			switch name {
			case "earlier":
				external.ExpiresAt--
			case "later":
				external.ExpiresAt++
			case "logout":
				disk = nil
			}
			writeCodexPendingDisk(t, f, disk)
			require.NoError(t, f.store.ReloadFromDisk(context.Background()))
			pc, ok := f.store.Config().Providers.Get(f.used.ID)
			if name == "logout" {
				assert.True(t, !ok || pc.OAuthToken == nil)
			} else {
				require.True(t, ok)
			}
			if name == "earlier" {
				assert.Equal(t, f.next, pc.OAuthToken)
			} else {
				assert.Equal(t, disk, pc.OAuthToken)
				configTestHooks.Lock()
				oldSync := configTestHooks.syncParent
				configTestHooks.syncParent = func(string) error { return errors.New("injected sync uncertainty") }
				configTestHooks.Unlock()
				t.Cleanup(func() { configTestHooks.Lock(); configTestHooks.syncParent = oldSync; configTestHooks.Unlock() })
				joinOAuthJournalTestStoresBeforeRestore(t)
				done, err := f.store.oauthJournalWorkerAttempt(context.Background(), ScopeGlobal, f.used.ID, 0)
				assert.False(t, done)
				assert.Error(t, err)
				assert.Same(t, entry, f.store.oauthOutboxCurrent(f.path, f.used.ID))
				configTestHooks.Lock()
				configTestHooks.syncParent = oldSync
				configTestHooks.Unlock()
			}
			failing.Store(false)
			for attempt := range 3 {
				done, err := f.store.oauthJournalWorkerAttempt(context.Background(), ScopeGlobal, f.used.ID, attempt)
				require.NoError(t, err)
				if done {
					break
				}
			}
			assert.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
			want := disk
			if name == "earlier" {
				want = f.next
			}
			if name == "logout" {
				pc, ok := f.store.Config().Providers.Get(f.used.ID)
				assert.True(t, !ok || pc.OAuthToken == nil)
				persisted, err := readOAuthJournalDiskToken(f.path, f.used.ID)
				require.NoError(t, err)
				assert.Nil(t, persisted)
			} else {
				assertCodexRefreshTokens(t, f, want, want)
			}
		})
	}
}

// Revert-check: later or tied external WAL wins only after verified durability.
func TestOAuthJournalOutbox_ExternalJournalArbitration(t *testing.T) {
	for _, name := range []string{"later", "tie"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			record := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(f.used.OAuthToken), Token: f.next}
			entry := f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, record)
			require.NoError(t, f.store.applyOAuthTokenWithPending(f.used.ID, f.used.OAuthToken, f.next, true))
			external := *f.next
			external.AccessToken, external.RefreshToken = "fixture-external-access", "fixture-external-refresh"
			if name == "later" {
				external.ExpiresAt++
			}
			other := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(f.used.OAuthToken), Token: &external}
			require.NoError(t, writeOAuthJournalOnce(oauthJournalPath(f.path, f.used.ID), other))
			configTestHooks.Lock()
			oldSync := configTestHooks.syncParent
			configTestHooks.syncParent = func(string) error { return errors.New("injected WAL sync uncertainty") }
			configTestHooks.Unlock()
			t.Cleanup(func() { configTestHooks.Lock(); configTestHooks.syncParent = oldSync; configTestHooks.Unlock() })
			joinOAuthJournalTestStoresBeforeRestore(t)
			done, err := f.store.oauthJournalWorkerAttempt(context.Background(), ScopeGlobal, f.used.ID, 0)
			assert.False(t, done)
			assert.Error(t, err)
			assert.Same(t, entry, f.store.oauthOutboxCurrent(f.path, f.used.ID))
			configTestHooks.Lock()
			configTestHooks.syncParent = oldSync
			configTestHooks.Unlock()
			for attempt := range 3 {
				done, err = f.store.oauthJournalWorkerAttempt(context.Background(), ScopeGlobal, f.used.ID, attempt)
				require.NoError(t, err)
				if done {
					break
				}
			}
			assert.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
			assertCodexRefreshTokens(t, f, &external, &external)
		})
	}
}

// Revert-check: raw reads log only the first error per file and failure kind.
func TestOAuthJournalOutbox_ReadErrorsLogFirstOnly(t *testing.T) {
	logs := captureCodexRefreshLogs(t)
	oldRead := oauthJournalReadFile
	oauthJournalReadFile = func(string) ([]byte, error) { return nil, os.ErrPermission }
	t.Cleanup(func() { oauthJournalReadFile = oldRead })
	path := filepath.Join(t.TempDir(), "pending.json")
	for range 4 {
		_, err := readOAuthJournal(path)
		require.ErrorIs(t, err, errOAuthJournalRead)
	}
	logs.mu.Lock()
	count := 0
	for _, record := range logs.records {
		if record.Message == "Failed to read OAuth journal" && record.Level == slog.LevelError {
			count++
		}
	}
	logs.mu.Unlock()
	assert.Equal(t, 1, count, "repeated raw read errors within 30s must log only the first failure")
}
