package config

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixtures keep workers blocked until cancellation and join before seam restoration.
func newOAuthJournalRefreshRecoveryFixture(t *testing.T) codexRefreshFixture {
	t.Helper()
	f := newCodexRefreshReloadFixture(t)
	old := oauthJournalBeforeWorker
	oauthJournalBeforeWorker = func(ctx context.Context) { <-ctx.Done() }
	t.Cleanup(func() { oauthJournalBeforeWorker = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
	return f
}

// Revert-check: skips valid B only when the caller actually used a different token A.
func TestOAuthJournalRefreshRecovery_ValidUsedBVersusControlA(t *testing.T) {
	for _, name := range []string{"same-used-B", "control-used-A"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			writeOAuthJournalFixture(t, f)
			used := f.used
			wantCalls := 0
			if name == "same-used-B" {
				used.OAuthToken = f.next
				f.store.SetProviderRuntimeConfig(used.ID, used)
				wantCalls = 1
			}
			c := *f.next
			c.AccessToken, c.RefreshToken, c.ExpiresAt = "rotated-C-access", "rotated-C-refresh", c.ExpiresAt+3600
			client := &http.Client{}
			calls := 0
			stubCodexRefresh(t, func(_ context.Context, actual *http.Client, token string) (*oauth.Token, error) {
				calls++
				assert.Same(t, client, actual)
				assert.Equal(t, f.next.RefreshToken, token)
				return &c, nil
			})
			err := f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, used, client)
			f.store.stopOAuthJournalWorkers()
			assert.NoError(t, err)
			assert.Equal(t, wantCalls, calls)
			pc, ok := f.store.Config().Providers.Get(used.ID)
			require.True(t, ok)
			if wantCalls == 1 {
				assert.Equal(t, &c, pc.OAuthToken)
				record, readErr := readOAuthJournal(oauthJournalPath(f.path, used.ID))
				require.NoError(t, readErr)
				require.NotNil(t, record)
				assert.Equal(t, &c, record.Token)
				assert.Equal(t, oauthRefreshTokenSHA256(used.OAuthToken), record.PredecessorRefreshTokenSHA256)
			} else {
				assert.Equal(t, f.next, pc.OAuthToken)
			}
		})
	}
}

// Revert-check: expired journal B exchanges through its caller-paired client despite corrupt config.
func TestOAuthJournalRefreshRecovery_ExpiredBCorruptConfigExchanges(t *testing.T) {
	f := newOAuthJournalRefreshRecoveryFixture(t)
	original, err := os.ReadFile(f.path)
	require.NoError(t, err)
	b := *f.next
	b.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	f.next = &b
	writeOAuthJournalFixture(t, f)
	corrupt := []byte("{invalid-config")
	require.NoError(t, os.WriteFile(f.path, corrupt, 0o600))
	c := b
	c.AccessToken, c.RefreshToken, c.ExpiresAt = "rotated-C-access", "rotated-C-refresh", time.Now().Add(time.Hour).Unix()
	client := &http.Client{}
	calls := 0
	stubCodexRefresh(t, func(_ context.Context, actual *http.Client, token string) (*oauth.Token, error) {
		calls++
		assert.Same(t, client, actual)
		assert.Equal(t, b.RefreshToken, token)
		return &c, nil
	})
	err = f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, client)
	f.store.stopOAuthJournalWorkers()
	assert.NoError(t, err)
	assert.Equal(t, 1, calls)
	pc, ok := f.store.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	assert.Equal(t, &c, pc.OAuthToken)
	data, err := os.ReadFile(f.path)
	require.NoError(t, err)
	assert.Equal(t, corrupt, data)
	record, err := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
	require.NoError(t, err)
	require.NotNil(t, record)
	assert.Equal(t, &c, record.Token)
	assert.Equal(t, oauthRefreshTokenSHA256(&b), record.PredecessorRefreshTokenSHA256)
	require.NoError(t, os.WriteFile(f.path, original, 0o600))
	repaired := newTestConfigStore(testStoreOpts{config: f.store.Config(), globalDataPath: f.path})
	repaired.workingDir = f.store.workingDir
	trackOAuthJournalTestStore(t, repaired)
	oauthJournalBeforeWorker = nil
	idle := hookOAuthJournalIdle(t, repaired)
	require.NoError(t, repaired.applyOAuthTokenWithPendingPredecessor(f.used.ID, &c, &c, &b, true))
	require.NoError(t, repaired.ReloadFromDisk(context.Background()))
	pc, ok = repaired.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	require.Equal(t, &c, pc.OAuthToken)
	repaired.startOAuthJournalWorker(ScopeGlobal, f.used.ID)
	waitOAuthJournalIdle(t, idle)
	persisted, err := readOAuthJournalDiskToken(f.path, f.used.ID)
	require.NoError(t, err)
	assert.Equal(t, &c, persisted)
	_, err = os.Stat(oauthJournalPath(f.path, f.used.ID))
	assert.True(t, os.IsNotExist(err))
}

// Revert-check: config-lock and equal/superseding sync uncertainty cannot block expired B exchange.
func TestOAuthJournalRefreshRecovery_ConfigUncertaintyExchangesB(t *testing.T) {
	for _, name := range []string{"config-lock", "equal-sync", "later-sync"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			b := *f.next
			b.ExpiresAt = time.Now().Add(-time.Minute).Unix()
			f.next = &b
			writeOAuthJournalFixture(t, f)
			if name != "config-lock" {
				disk := b
				if name == "later-sync" {
					disk.AccessToken, disk.RefreshToken, disk.ExpiresAt = "external-access", "external-refresh", b.ExpiresAt+1
				}
				writeCodexPendingDisk(t, f, &disk)
				configTestHooks.Lock()
				old := configTestHooks.syncParent
				failed := false
				configTestHooks.syncParent = func(path string) error {
					if !failed {
						failed = true
						return errors.New("config sync uncertainty")
					}
					return oldSyncOAuthJournalParent(path, old)
				}
				configTestHooks.Unlock()
				t.Cleanup(func() { configTestHooks.Lock(); configTestHooks.syncParent = old; configTestHooks.Unlock() })
				joinOAuthJournalTestStoresBeforeRestore(t)
			} else {
				hookCodexRefreshLock(t, func(ctx context.Context, path string) (*filelock.FileLock, error) {
					if path == normalizeReloadPath(f.path)+".lock" {
						return nil, errors.New("config lock timeout")
					}
					return filelock.AcquireFileLockContext(ctx, path)
				})
			}
			c := b
			c.AccessToken, c.RefreshToken, c.ExpiresAt = "rotated-C-access", "rotated-C-refresh", time.Now().Add(time.Hour).Unix()
			client := &http.Client{}
			calls := 0
			stubCodexRefresh(t, func(ctx context.Context, actual *http.Client, token string) (*oauth.Token, error) {
				calls++
				assert.NoError(t, ctx.Err())
				assert.Same(t, client, actual)
				assert.Equal(t, b.RefreshToken, token)
				acquired := f.store.diskWriteMu.TryLock()
				assert.True(t, acquired)
				if acquired {
					f.store.diskWriteMu.Unlock()
				}
				return &c, nil
			})
			err := f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, client)
			f.store.stopOAuthJournalWorkers()
			assert.NoError(t, err)
			assert.Equal(t, 1, calls)
			record, err := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
			require.NoError(t, err)
			require.NotNil(t, record)
			assert.Equal(t, &c, record.Token)
			assert.Equal(t, oauthRefreshTokenSHA256(&b), record.PredecessorRefreshTokenSHA256)
		})
	}
}

// Revert-check: persistent journal I/O keeps identical WAL bytes, forbids exchange, and is not completion or absence.
func TestOAuthJournalRefreshRecovery_PersistentReadIO(t *testing.T) {
	for _, entry := range []string{"leader", "worker", "load"} {
		t.Run(entry, func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			writeOAuthJournalFixture(t, f)
			path := oauthJournalPath(f.path, f.used.ID)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			oldWait := oauthJournalReadWait
			oauthJournalReadWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
			t.Cleanup(func() { oauthJournalReadWait = oldWait })
			joinOAuthJournalTestStoresBeforeRestore(t)
			old := oauthJournalReadFile
			reads := 0
			oauthJournalReadFile = func(name string) ([]byte, error) {
				if name == path {
					reads++
					return nil, &os.PathError{Op: "read", Path: name, Err: os.ErrPermission}
				}
				return old(name)
			}
			t.Cleanup(func() { oauthJournalReadFile = old })
			joinOAuthJournalTestStoresBeforeRestore(t)
			calls := 0
			stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
				calls++
				return f.next, nil
			})
			switch entry {
			case "leader":
				err = f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil)
				assert.Error(t, err)
			case "worker":
				done, attemptErr := f.store.oauthJournalWorkerAttempt(context.Background(), ScopeGlobal, f.used.ID, 0)
				assert.False(t, done)
				assert.Error(t, attemptErr)
			case "load":
				loaded, loadErr := Load(f.store.workingDir, "", false)
				require.NoError(t, loadErr)
				trackOAuthJournalTestStore(t, loaded)
				loaded.oauthWorkerMu.Lock()
				assert.NotNil(t, loaded.oauthWorkers[f.used.ID])
				loaded.oauthWorkerMu.Unlock()
				loaded.stopOAuthJournalWorkers()
			}
			f.store.stopOAuthJournalWorkers()
			assert.Positive(t, reads)
			assert.Zero(t, calls)
			after, readErr := os.ReadFile(path)
			assert.NoError(t, readErr)
			assert.Equal(t, before, after)
			quarantines, globErr := filepath.Glob(path + ".corrupt-*")
			require.NoError(t, globErr)
			assert.Empty(t, quarantines)
		})
	}
}

// Revert-check: transient journal I/O retries without quarantine and adopts B without exchange.
func TestOAuthJournalRefreshRecovery_TransientReadIO(t *testing.T) {
	f := newOAuthJournalRefreshRecoveryFixture(t)
	writeOAuthJournalFixture(t, f)
	path := oauthJournalPath(f.path, f.used.ID)
	oldRead, oldWait := oauthJournalReadFile, oauthJournalReadWait
	reads := 0
	var waits []time.Duration
	oauthJournalReadFile = func(name string) ([]byte, error) {
		if name == path {
			reads++
			if reads <= 2 {
				return nil, os.ErrPermission
			}
		}
		return oldRead(name)
	}
	oauthJournalReadWait = func(ctx context.Context, delay time.Duration) error {
		waits = append(waits, delay)
		return ctx.Err()
	}
	t.Cleanup(func() { oauthJournalReadFile, oauthJournalReadWait = oldRead, oldWait })
	joinOAuthJournalTestStoresBeforeRestore(t)
	calls := 0
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		calls++
		return nil, nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	f.store.stopOAuthJournalWorkers()
	assert.Equal(t, 3, reads)
	assert.Equal(t, oauthJournalReadBackoffs[:2], waits)
	assert.Zero(t, calls)
	pc, ok := f.store.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	assert.Equal(t, f.next, pc.OAuthToken)
	quarantines, err := filepath.Glob(path + ".corrupt-*")
	require.NoError(t, err)
	assert.Empty(t, quarantines)
}

// Revert-check: a live worker retains its scope after journal I/O and completes after repair.
func TestOAuthJournalRefreshRecovery_WorkerReadIOKeepsScope(t *testing.T) {
	f := newOAuthJournalRefreshRecoveryFixture(t)
	writeOAuthJournalFixture(t, f)
	path := oauthJournalPath(f.path, f.used.ID)
	oldRead, oldReadWait, oldWorkerWait := oauthJournalReadFile, oauthJournalReadWait, oauthJournalWorkerWait
	oauthJournalReadFile = func(name string) ([]byte, error) {
		if name == path {
			return nil, os.ErrPermission
		}
		return oldRead(name)
	}
	oauthJournalReadWait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	waiting, resume := make(chan struct{}), make(chan struct{})
	oauthJournalWorkerWait = func(ctx context.Context, _ time.Duration) error {
		close(waiting)
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() {
		oauthJournalReadFile, oauthJournalReadWait, oauthJournalWorkerWait = oldRead, oldReadWait, oldWorkerWait
	})
	joinOAuthJournalTestStoresBeforeRestore(t)
	oauthJournalBeforeWorker = nil
	idle := hookOAuthJournalIdle(t, f.store)
	f.store.startOAuthJournalWorker(ScopeGlobal, f.used.ID)
	awaitCodexSignal(t, waiting)
	f.store.oauthWorkerMu.Lock()
	worker := f.store.oauthWorkers[f.used.ID]
	require.NotNil(t, worker)
	assert.NotZero(t, worker.scopes[ScopeGlobal])
	f.store.oauthWorkerMu.Unlock()
	// Channel release orders seam repair before the worker's next attempt.
	oauthJournalReadFile = oldRead
	oauthJournalWorkerWait = oldWorkerWait
	close(resume)
	waitOAuthJournalIdle(t, idle)
	f.store.stopOAuthJournalWorkers()
	assertCodexRefreshTokens(t, f, f.next, f.next)
	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

// Revert-check: supersession and logout must retain WAL and pending state when config-parent sync is uncertain.
func TestOAuthJournalRefreshRecovery_SupersessionSyncFailureRetains(t *testing.T) {
	for _, name := range []string{"equal", "later", "tie", "logout"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			writeOAuthJournalFixture(t, f)
			require.NoError(t, f.store.applyOAuthTokenWithPending(f.used.ID, f.used.OAuthToken, f.next, true))
			disk := f.next
			switch name {
			case "later", "tie":
				c := *f.next
				c.AccessToken, c.RefreshToken = "external-access", "external-refresh"
				if name == "later" {
					c.ExpiresAt++
				}
				disk = &c
			case "logout":
				disk = nil
			}
			writeCodexRefreshDisk(t, f.path, disk)
			path := oauthJournalPath(f.path, f.used.ID)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			configTestHooks.Lock()
			old := configTestHooks.syncParent
			configTestHooks.syncParent = func(string) error { return errors.New("injected config-parent uncertainty") }
			configTestHooks.Unlock()
			t.Cleanup(func() {
				configTestHooks.Lock()
				configTestHooks.syncParent = old
				configTestHooks.Unlock()
			})
			joinOAuthJournalTestStoresBeforeRestore(t)
			done, attemptErr := f.store.oauthJournalWorkerAttempt(context.Background(), ScopeGlobal, f.used.ID, 0)
			assert.False(t, done)
			assert.Error(t, attemptErr)
			after, readErr := os.ReadFile(path)
			assert.NoError(t, readErr)
			assert.Equal(t, before, after)
			f.store.publishMu.Lock()
			_, pending := f.store.pendingOAuth[f.used.ID]
			f.store.publishMu.Unlock()
			assert.True(t, pending)
		})
	}
}
