package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func newCodexRefreshReloadFixture(t *testing.T) codexRefreshFixture {
	t.Helper()
	_, dataDir := isolateAllGlobalConfigPaths(t)
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	resetProviderState()
	t.Cleanup(resetProviderState)
	available := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return nil }
	t.Cleanup(func() { cliprovider.AvailableFunc = available })

	root := t.TempDir()
	old := &oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresIn: 3600, ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "test-account"}
	next := &oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: old.AccountID}
	pc := ProviderConfig{ID: "openai-codex", Disable: true, OAuthToken: old}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected fixture discovery request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	discoverModels := false
	cfg := &Config{
		Options: &Options{DisableDefaultProviders: true, DisableProviderAutoUpdate: true, DataDirectory: filepath.Join(root, ".rush")},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			pc.ID: pc,
			"fixture-custom": {
				ID: "fixture-custom", BaseURL: server.URL, APIKey: "fixture-test-key",
				AutoDiscoverModels: &discoverModels,
				Models:             []catwalk.Model{{ID: "fixture-model", Name: "Fixture model", ContextWindow: 8192, DefaultMaxTokens: 1024}},
			},
		}),
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)
	path := filepath.Join(dataDir, "rush.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	store := newTestConfigStore(testStoreOpts{config: cfg, globalDataPath: path})
	store.workingDir = root
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	used, ok := store.Config().Providers.Get(pc.ID)
	require.True(t, ok)
	require.True(t, used.Disable)
	require.Equal(t, old, used.OAuthToken)
	trackOAuthJournalTestStore(t, store)
	return codexRefreshFixture{store: store, path: path, used: used, next: next}
}

// Revert-check: predecessor reload must preserve B until persistence completes.
func TestOAuthRefresh_ReloadAfterMemoryPublishRetainsPendingToken(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	idle := hookOAuthJournalIdle(t, f.store)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	persist := make(chan struct{})
	allowPersist := make(chan struct{})
	var entered, released sync.Once
	hookCodexPendingPersist(t, func(ctx context.Context, _ int) error {
		entered.Do(func() { close(persist) })
		select {
		case <-allowPersist:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	result := startCodexRefresh(t, ctx, f)
	t.Cleanup(func() {
		cancel()
		released.Do(func() { close(allowPersist) })
	})
	awaitCodexSignal(t, persist)
	assertCodexPending(t, f, true)
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)

	require.NoError(t, f.store.ReloadFromDisk(ctx))
	pc, ok := f.store.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	assert.Equal(t, f.next, pc.OAuthToken, "reload from predecessor A must retain pending rotation B")
	disk, err := f.store.loadTokenFromDisk(ScopeGlobal, f.used.ID)
	require.NoError(t, err)
	require.Equal(t, f.used.OAuthToken, disk)

	released.Do(func() { close(allowPersist) })
	require.NoError(t, awaitCodexResult(t, result))
	waitOAuthJournalIdle(t, idle)
	assertCodexPending(t, f, false)
	disk, err = f.store.loadTokenFromDisk(ScopeGlobal, f.used.ID)
	require.NoError(t, err)
	assert.Equal(t, f.next, disk, "pending rotation must reach disk after persistence resumes")
	require.NoError(t, f.store.ReloadFromDisk(ctx))
	pc, ok = f.store.Config().Providers.Get(f.used.ID)
	require.True(t, ok)
	assert.Equal(t, f.next, pc.OAuthToken, "reload after commit must retain B")
	assertCodexPending(t, f, false)
}

func hookCodexPendingPersist(t *testing.T, fn func(context.Context, int) error) {
	t.Helper()
	old := codexRefreshBeforePersist
	codexRefreshBeforePersist = fn
	t.Cleanup(func() { codexRefreshBeforePersist = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
}

func assertCodexPending(t *testing.T, f codexRefreshFixture, exists bool) {
	t.Helper()
	f.store.publishMu.Lock()
	entry, ok := f.store.pendingOAuth[f.used.ID]
	f.store.publishMu.Unlock()
	require.Equal(t, exists, ok)
	if exists {
		require.Equal(t, *f.used.OAuthToken, entry.predecessor)
		require.Equal(t, *f.next, entry.token)
	}
}

func writeCodexPendingDisk(t *testing.T, f codexRefreshFixture, token *oauth.Token) {
	t.Helper()
	data, err := os.ReadFile(f.path)
	require.NoError(t, err)
	var updated string
	if token == nil {
		updated, err = sjson.Delete(string(data), "providers.openai-codex.oauth")
	} else {
		updated, err = sjson.Set(string(data), "providers.openai-codex.oauth", token)
	}
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.path, []byte(updated), 0o600))
}

func pauseCodexPendingPersist(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	entered := make(chan struct{})
	resume := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	hookCodexPendingPersist(t, func(ctx context.Context, _ int) error {
		enterOnce.Do(func() { close(entered) })
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	return entered, release
}

// Revert-check: retrying persistence must not lose B to a predecessor reload.
func TestOAuthRefresh_PendingTransientRetrySurvivesReload(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	idle := hookOAuthJournalIdle(t, f.store)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	hookOAuthJournalFastRetry(t)
	hookCodexPendingPersist(t, func(ctx context.Context, attempt int) error {
		if attempt == 0 {
			return errors.New("transient persistence failure")
		}
		assertCodexPending(t, f, true)
		require.NoError(t, f.store.ReloadFromDisk(ctx))
		assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
		return nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, f.next, f.next)
	assertCodexPending(t, f, false)
}

// Revert-check: reload must adopt C or logout and prevent stale B persistence.
func TestOAuthRefresh_PendingExternalReloadWins(t *testing.T) {
	for _, logout := range []bool{false, true} {
		name := "rotation"
		if logout {
			name = "logout"
		}
		t.Run(name, func(t *testing.T) {
			f := newCodexRefreshReloadFixture(t)
			idle := hookOAuthJournalIdle(t, f.store)
			stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
			entered, release := pauseCodexPendingPersist(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			result := startCodexRefresh(t, ctx, f)
			t.Cleanup(func() { cancel(); release() })
			awaitCodexSignal(t, entered)
			assertCodexPending(t, f, true)
			var external *oauth.Token
			if !logout {
				c := *f.next
				c.AccessToken, c.RefreshToken = "external-access", "external-refresh"
				c.ExpiresAt++
				external = &c
			}
			writeCodexPendingDisk(t, f, external)
			require.NoError(t, f.store.ReloadFromDisk(ctx))
			assertCodexPending(t, f, false)
			pc, ok := f.store.Config().Providers.Get(f.used.ID)
			if logout {
				require.True(t, !ok || pc.OAuthToken == nil)
			} else {
				require.True(t, ok)
				require.Equal(t, external, pc.OAuthToken)
			}
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			release()
			require.NoError(t, awaitCodexResult(t, result))
			waitOAuthJournalIdle(t, idle)
			after, err := os.ReadFile(f.path)
			require.NoError(t, err)
			require.Equal(t, before, after)
			assertCodexPending(t, f, false)
			require.NoError(t, f.store.ReloadFromDisk(ctx))
			pc, ok = f.store.Config().Providers.Get(f.used.ID)
			if logout {
				require.True(t, !ok || pc.OAuthToken == nil)
			} else {
				require.Equal(t, external, pc.OAuthToken)
			}
		})
	}
}

// Revert-check: equal disk bytes retain pending protection until durability verification.
func TestOAuthRefresh_PendingSameDiskTokenClearsOnReload(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	idle := hookOAuthJournalIdle(t, f.store)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	entered, release := pauseCodexPendingPersist(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	result := startCodexRefresh(t, ctx, f)
	t.Cleanup(func() { cancel(); release() })
	awaitCodexSignal(t, entered)
	assertCodexPending(t, f, true)
	writeCodexPendingDisk(t, f, f.next)
	require.NoError(t, f.store.ReloadFromDisk(ctx))
	assertCodexPending(t, f, true)
	release()
	require.NoError(t, awaitCodexResult(t, result))
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, f.next, f.next)
}

// Revert-check: a candidate built before B publishes must be rejected and rebuilt.
func TestOAuthRefresh_PrePublishReloadRebuildsPendingToken(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	idle := hookOAuthJournalIdle(t, f.store)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	persist, releasePersist := pauseCodexPendingPersist(t)
	candidate := make(chan struct{})
	resume := make(chan struct{})
	var releaseOnce sync.Once
	releaseReload := func() { releaseOnce.Do(func() { close(resume) }) }
	builds := 0
	f.store.reloadBeforePublish = func() {
		builds++
		if builds == 1 {
			close(candidate)
			<-resume
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	reloadResult := make(chan error, 1)
	reloadDone := make(chan struct{})
	go func() {
		defer close(reloadDone)
		reloadResult <- f.store.ReloadFromDisk(ctx)
	}()
	t.Cleanup(func() { awaitCodexSignal(t, reloadDone) })
	t.Cleanup(func() { cancel(); releaseReload(); releasePersist() })
	awaitCodexSignal(t, candidate)
	result := startCodexRefresh(t, ctx, f)
	t.Cleanup(func() { cancel(); releaseReload(); releasePersist() })
	awaitCodexSignal(t, persist)
	assertCodexPending(t, f, true)
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	releaseReload()
	require.NoError(t, awaitCodexResult(t, reloadResult))
	require.GreaterOrEqual(t, builds, 2)
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	assertCodexPending(t, f, true)
	releasePersist()
	require.NoError(t, awaitCodexResult(t, result))
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, f.next, f.next)
	assertCodexPending(t, f, false)
}

// Revert-check: real polling must exhaust its budget and preserve the exchange error.
func TestOAuthRefresh_RealPollingBudgetPreservesRejection(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	rejected := &oauth.TokenExchangeError{StatusCode: 400, Body: "invalid_grant"}
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return nil, rejected })
	budget, interval := codexRefreshPollBudget, codexRefreshPollInterval
	codexRefreshPollBudget, codexRefreshPollInterval = 80*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { codexRefreshPollBudget, codexRefreshPollInterval = budget, interval })
	start := time.Now()
	err := f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil)
	elapsed := time.Since(start)
	require.ErrorIs(t, err, rejected)
	require.GreaterOrEqual(t, elapsed, 80*time.Millisecond)
	require.Less(t, elapsed, 2*time.Second)
	assertCodexRefreshTokens(t, f, f.used.OAuthToken, f.used.OAuthToken)
	assertCodexPending(t, f, false)
}

// Revert-check: cancellation must interrupt an actual contended refresh sidecar.
func TestOAuthRefresh_RealContendedRefreshLockCancellation(t *testing.T) {
	f := newCodexRefreshReloadFixture(t)
	digest := sha256.Sum256([]byte(f.used.ID))
	lockPath := fmt.Sprintf("%s.oauth-refresh-%x.lock", normalizeReloadPath(f.path), digest[:8])
	lock, err := acquireConfigFileLock(context.Background(), lockPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, releaseConfigFileLock(lockPath, lock)) })
	attempted := make(chan struct{})
	var once sync.Once
	hookCodexRefreshLock(t, func(ctx context.Context, path string) (*filelock.FileLock, error) {
		if path == lockPath {
			once.Do(func() { close(attempted) })
		}
		return filelock.AcquireFileLockContext(ctx, path)
	})
	calls := 0
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { calls++; return f.next, nil })
	ctx, cancel := context.WithCancel(context.Background())
	result := startCodexRefresh(t, ctx, f)
	t.Cleanup(cancel)
	awaitCodexSignal(t, attempted)
	select {
	case err := <-result:
		t.Fatalf("refresh escaped held sidecar: %v", err)
	default:
	}
	cancelStarted := time.Now()
	cancel()
	require.ErrorIs(t, awaitCodexResult(t, result), context.Canceled)
	require.Less(t, time.Since(cancelStarted), time.Second)
	require.Zero(t, calls)
	assertCodexRefreshTokens(t, f, f.used.OAuthToken, f.used.OAuthToken)
	assertCodexPending(t, f, false)
}

// Revert-check: failure and giveup must end pending protection without rolling back B.
func TestOAuthRefresh_PendingFailureCleanup(t *testing.T) {
	t.Run("memory-CAS", func(t *testing.T) {
		f := newCodexRefreshReloadFixture(t)
		stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
			pc := f.used
			pc.OAuthToken = nil
			f.store.SetProviderRuntimeConfig(pc.ID, pc)
			return f.next, nil
		})
		require.Error(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
		f.store.stopOAuthJournalWorkers()
		requireOAuthJournalToken(t, f)
		assertCodexPending(t, f, false)
	})
	t.Run("store-cancellation", func(t *testing.T) {
		f := newCodexRefreshReloadFixture(t)
		stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
		entered, release := pauseCodexPendingPersist(t)
		t.Cleanup(release)
		require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
		awaitCodexSignal(t, entered)
		f.store.stopOAuthJournalWorkers()
		requireOAuthJournalToken(t, f)
		assertCodexPending(t, f, true)
		require.NoError(t, f.store.ReloadFromDisk(context.Background()))
		assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	})
}
