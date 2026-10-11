package config

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type codexRefreshFixture struct {
	store *ConfigStore
	path  string
	used  ProviderConfig
	next  *oauth.Token
}

func newCodexRefreshFixture(t *testing.T) codexRefreshFixture {
	t.Helper()
	old := &oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresIn: 3600, ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "test-account"}
	next := &oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix(), AccountID: old.AccountID}
	pc := ProviderConfig{ID: "openai-codex", OAuthToken: old}
	path := filepath.Join(t.TempDir(), "rush.json")
	writeCodexRefreshDisk(t, path, old)
	store := newTestConfigStore(testStoreOpts{config: &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{pc.ID: pc})}, globalDataPath: path})
	trackOAuthJournalTestStore(t, store)
	return codexRefreshFixture{store, path, pc, next}
}

func writeCodexRefreshDisk(t *testing.T, path string, token *oauth.Token) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"providers": map[string]any{"openai-codex": map[string]any{"oauth": token}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func stubCodexRefresh(t *testing.T, fn func(context.Context, *http.Client, string) (*oauth.Token, error)) {
	t.Helper()
	old := codexRefreshTokenFn
	codexRefreshTokenFn = fn
	t.Cleanup(func() { codexRefreshTokenFn = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
}

func hookCodexRefreshLock(t *testing.T, fn func(context.Context, string) (*filelock.FileLock, error)) {
	t.Helper()
	configTestHooks.Lock()
	old := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = fn
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = old
		configTestHooks.Unlock()
	})
	joinOAuthJournalTestStoresBeforeRestore(t)
}

func awaitCodexSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("missing refresh handshake")
	}
}

func awaitCodexResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish")
		return nil
	}
}

func startCodexRefresh(t *testing.T, ctx context.Context, f codexRefreshFixture) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, f.used, nil)
	}()
	t.Cleanup(func() { awaitCodexSignal(t, done) })
	joinOAuthJournalTestStoresBeforeRestore(t)
	return result
}

func assertCodexRefreshTokens(t *testing.T, f codexRefreshFixture, memory, disk *oauth.Token) {
	t.Helper()
	pc, ok := f.store.loadSnapshot().config.Providers.Get(f.used.ID)
	require.True(t, ok)
	require.Equal(t, memory, pc.OAuthToken)
	require.Empty(t, pc.APIKey)
	actual, err := f.store.loadTokenFromDisk(ScopeGlobal, f.used.ID)
	require.NoError(t, err)
	require.Equal(t, disk, actual)
}

type codexRefreshLogs struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *codexRefreshLogs) Enabled(context.Context, slog.Level) bool { return true }
func (h *codexRefreshLogs) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *codexRefreshLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *codexRefreshLogs) WithGroup(string) slog.Handler      { return h }

func captureCodexRefreshLogs(t *testing.T) *codexRefreshLogs {
	t.Helper()
	h := &codexRefreshLogs{}
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })
	joinOAuthJournalTestStoresBeforeRestore(t)
	return h
}

func (h *codexRefreshLogs) requireError(t *testing.T, message string) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if r.Message == message && r.Level == slog.LevelError {
			return
		}
	}
	t.Errorf("missing Error log %q", message)
}

// Revert-check: codexRefreshAlreadyCurrent (production :54) must compare used A with valid B.
func TestOAuthRefresh_DifferentValidMemorySkipsExchange(t *testing.T) {
	f := newCodexRefreshFixture(t)
	pc := f.used
	pc.OAuthToken = f.next
	f.store.SetProviderRuntimeConfig(pc.ID, pc)
	calls := 0
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { calls++; return f.next, nil })
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	require.Zero(t, calls)
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
}

// Revert-check: codexRefreshAlreadyCurrent (production :54) must not skip the same valid token.
func TestOAuthRefresh_SameValidUsedTokenExchanges(t *testing.T) {
	f := newCodexRefreshFixture(t)
	idle := hookOAuthJournalIdle(t, f.store)
	old := *f.used.OAuthToken
	old.ExpiresAt = time.Now().Add(time.Hour).Unix()
	f.used.OAuthToken = &old
	f.store.SetProviderRuntimeConfig(f.used.ID, f.used)
	writeCodexRefreshDisk(t, f.path, &old)
	calls := 0
	client := &http.Client{}
	stubCodexRefresh(t, func(_ context.Context, actual *http.Client, token string) (*oauth.Token, error) {
		calls++
		require.Same(t, client, actual)
		require.Equal(t, old.RefreshToken, token)
		return f.next, nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, client))
	require.Equal(t, 1, calls)
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, f.next, f.next)
}

// Both callers return before blocked config persistence.
func TestOAuthRefresh_MemoryFirstWaiterReturnsBeforePersist(t *testing.T) {
	f := newCodexRefreshFixture(t)
	idle := hookOAuthJournalIdle(t, f.store)
	exchange := make(chan struct{})
	allowExchange := make(chan struct{})
	persist := make(chan struct{})
	allowPersist := make(chan struct{})
	waiting := make(chan struct{})
	var waiterOnce sync.Once
	oldHook := codexRefreshWaiterHook
	codexRefreshWaiterHook = func() { waiterOnce.Do(func() { close(waiting) }) }
	t.Cleanup(func() { codexRefreshWaiterHook = oldHook })
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		close(exchange)
		<-allowExchange
		return f.next, nil
	})
	var persistOnce sync.Once
	hookCodexPendingPersist(t, func(ctx context.Context, _ int) error {
		persistOnce.Do(func() { close(persist) })
		select {
		case <-allowPersist:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	leader := startCodexRefresh(t, ctx, f)
	awaitCodexSignal(t, exchange)
	waiter := startCodexRefresh(t, ctx, f)
	var releaseExchange, releasePersist sync.Once
	t.Cleanup(func() {
		cancel()
		releaseExchange.Do(func() { close(allowExchange) })
		releasePersist.Do(func() { close(allowPersist) })
	})
	awaitCodexSignal(t, waiting)
	releaseExchange.Do(func() { close(allowExchange) })
	awaitCodexSignal(t, persist)
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	require.NoError(t, awaitCodexResult(t, waiter))
	select {
	case err := <-leader:
		require.NoError(t, err)
	default:
		require.NoError(t, awaitCodexResult(t, leader))
	}
	releasePersist.Do(func() { close(allowPersist) })
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, f.next, f.next)
}

// Config persistence retries beyond the former bounded budget.
func TestOAuthRefresh_PersistIOExhaustion(t *testing.T) {
	f := newCodexRefreshFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	idle := hookOAuthJournalIdle(t, f.store)
	hookOAuthJournalFastRetry(t)
	failed := make(chan struct{})
	terminated := make(chan struct{}, 1)
	oldDone := oauthJournalWorkerDone
	oauthJournalWorkerDone = func(store *ConfigStore, _ string) {
		if store == f.store {
			terminated <- struct{}{}
		}
	}
	t.Cleanup(func() { oauthJournalWorkerDone = oldDone })
	joinOAuthJournalTestStoresBeforeRestore(t)
	var attemptCount atomic.Int32
	resume := make(chan struct{})
	var once sync.Once
	hookCodexPendingPersist(t, func(ctx context.Context, attempt int) error {
		attemptCount.Add(1)
		if attempt < 10 {
			return errors.New("injected config IO")
		}
		once.Do(func() { close(failed) })
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	t.Cleanup(func() { close(resume) })
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	select {
	case <-failed:
	case <-terminated:
		require.GreaterOrEqual(t, attemptCount.Load(), int32(11), "persister stopped at the old retry limit")
	case <-time.After(5 * time.Second):
		t.Fatal("persister neither retried nor terminated")
	}
	requireOAuthJournalToken(t, f)
	assertCodexPending(t, f, true)
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	resume <- struct{}{}
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, f.next, f.next)
	assertCodexPending(t, f, false)
}

// A transient config failure must eventually recover.
func TestOAuthRefresh_PersistTransientRecovery(t *testing.T) {
	f := newCodexRefreshFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	idle := hookOAuthJournalIdle(t, f.store)
	hookOAuthJournalFastRetry(t)
	var attempts atomic.Int32
	hookCodexPendingPersist(t, func(context.Context, int) error {
		if attempts.Add(1) == 1 {
			return errors.New("transient config IO")
		}
		return nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	waitOAuthJournalIdle(t, idle)
	require.Equal(t, int32(2), attempts.Load())
	assertCodexRefreshTokens(t, f, f.next, f.next)
}

// Uncertain config durability must not discard the journal.
func TestOAuthRefresh_CommittedUncertainty(t *testing.T) {
	f := newCodexRefreshFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	idle := hookOAuthJournalIdle(t, f.store)
	entered, release := pauseCodexPendingPersist(t)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	var failures atomic.Int32
	configTestHooks.Lock()
	old := configTestHooks.syncParent
	configTestHooks.syncParent = func(path string) error {
		if failures.Add(1) <= 2 {
			return errors.New("injected durability uncertainty")
		}
		return oldSyncOAuthJournalParent(path, old)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() { configTestHooks.Lock(); configTestHooks.syncParent = old; configTestHooks.Unlock() })
	joinOAuthJournalTestStoresBeforeRestore(t)
	t.Cleanup(release)
	requireOAuthJournalToken(t, f)
	release()
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, f.next, f.next)
}

// Equal-expiry disk replacements win arbitration.
func TestOAuthRefresh_DiskConflictAfterMemoryPublishWins(t *testing.T) {
	f := newCodexRefreshFixture(t)
	replacement := *f.next
	replacement.AccessToken = "external-access"
	replacement.RefreshToken = "external-refresh"
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	idle := hookOAuthJournalIdle(t, f.store)
	var once sync.Once
	hookCodexPendingPersist(t, func(context.Context, int) error {
		once.Do(func() { writeCodexRefreshDisk(t, f.path, &replacement) })
		return nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	waitOAuthJournalIdle(t, idle)
	assertCodexRefreshTokens(t, f, &replacement, &replacement)
}

// Revert-check: pollCodexRefreshDisk (:171) must reread after a rejected exchange.
func TestOAuthRefresh_RejectionDelayedDiskAdoption(t *testing.T) {
	f := newCodexRefreshFixture(t)
	rejected := &oauth.TokenExchangeError{StatusCode: 400, Body: "invalid_grant"}
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return nil, rejected })
	polling := make(chan struct{})
	resume := make(chan struct{})
	old := codexRefreshPollWait
	codexRefreshPollWait = func(ctx context.Context, _ time.Duration) error {
		close(polling)
		select {
		case <-resume:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.Cleanup(func() { codexRefreshPollWait = old })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result := startCodexRefresh(t, ctx, f)
	t.Cleanup(cancel)
	awaitCodexSignal(t, polling)
	writeCodexRefreshDisk(t, f.path, f.next)
	close(resume)
	require.NoError(t, awaitCodexResult(t, result))
	assertCodexRefreshTokens(t, f, f.next, f.next)
}

// Revert-check: pollCodexRefreshDisk (:171) must preserve the original rejection on exhaustion.
func TestOAuthRefresh_RejectionNoUpdatePreservesError(t *testing.T) {
	f := newCodexRefreshFixture(t)
	rejected := &oauth.TokenExchangeError{StatusCode: 400, Body: "invalid_grant"}
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return nil, rejected })
	old := codexRefreshPollWait
	codexRefreshPollWait = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	t.Cleanup(func() { codexRefreshPollWait = old })
	err := f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil)
	require.ErrorIs(t, err, rejected)
	assertCodexRefreshTokens(t, f, f.used.OAuthToken, f.used.OAuthToken)
}

// Revert-check: refreshCodexOAuthToken (:63) must cancel a slot waiter without exchanging.
func TestOAuthRefresh_CanceledSingleflightWaiter(t *testing.T) {
	f := newCodexRefreshFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	waiting := make(chan struct{})
	var once sync.Once
	old := codexRefreshWaiterHook
	codexRefreshWaiterHook = func() { once.Do(func() { close(waiting) }) }
	t.Cleanup(func() { codexRefreshWaiterHook = old })
	var calls atomic.Int32
	stubCodexRefresh(t, func(ctx context.Context, _ *http.Client, _ string) (*oauth.Token, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			return f.next, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	leader := startCodexRefresh(t, ctx, f)
	t.Cleanup(cancel)
	awaitCodexSignal(t, entered)
	waitCtx, waitCancel := context.WithCancel(ctx)
	waiter := startCodexRefresh(t, waitCtx, f)
	t.Cleanup(waitCancel)
	awaitCodexSignal(t, waiting)
	waitCancel()
	require.ErrorIs(t, awaitCodexResult(t, waiter), context.Canceled)
	require.Equal(t, int32(1), calls.Load())
	close(release)
	require.NoError(t, awaitCodexResult(t, leader))
	codexRefreshActive.Lock()
	_, exists := codexRefreshActive.calls[codexRefreshKey{f.store, f.used.ID}]
	codexRefreshActive.Unlock()
	require.False(t, exists)
}

// Revert-check: refreshCodexOAuthLeader (:103) must cancel refresh-sidecar waiting before exchange.
func TestOAuthRefresh_CanceledRefreshSidecarWaiter(t *testing.T) {
	f := newCodexRefreshFixture(t)
	entered := make(chan struct{})
	var calls atomic.Int32
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { calls.Add(1); return f.next, nil })
	hookCodexRefreshLock(t, func(ctx context.Context, path string) (*filelock.FileLock, error) {
		if strings.Contains(path, ".oauth-refresh-") {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return filelock.AcquireFileLockContext(ctx, path)
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := startCodexRefresh(t, ctx, f)
	t.Cleanup(cancel)
	awaitCodexSignal(t, entered)
	cancel()
	require.ErrorIs(t, awaitCodexResult(t, result), context.Canceled)
	require.Zero(t, calls.Load())
	assertCodexRefreshTokens(t, f, f.used.OAuthToken, f.used.OAuthToken)
}

// Revert-check: withCodexConfigWriteLock (:263) must cancel disk wait but retain published memory.
func TestOAuthRefresh_CanceledDiskWaitRetainsToken(t *testing.T) {
	f := newCodexRefreshFixture(t)
	stubCodexRefresh(t, func(context.Context, *http.Client, string) (*oauth.Token, error) { return f.next, nil })
	idle := hookOAuthJournalIdle(t, f.store)
	terminated := make(chan struct{}, 1)
	oldDone := oauthJournalWorkerDone
	oauthJournalWorkerDone = func(store *ConfigStore, _ string) {
		if store == f.store {
			terminated <- struct{}{}
		}
	}
	t.Cleanup(func() { oauthJournalWorkerDone = oldDone })
	joinOAuthJournalTestStoresBeforeRestore(t)
	entered, release := pauseCodexPendingPersist(t)
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, f.used, nil))
	awaitCodexSignal(t, entered)
	cancel()
	requireOAuthJournalToken(t, f)
	assertCodexPending(t, f, true)
	release()
	select {
	case <-idle:
	case <-terminated:
		disk, err := f.store.loadTokenFromDisk(ScopeGlobal, f.used.ID)
		require.NoError(t, err)
		require.Equal(t, f.next, disk, "caller cancellation stopped persistence")
	case <-time.After(5 * time.Second):
		t.Fatal("persister neither completed nor terminated")
	}
	assertCodexRefreshTokens(t, f, f.next, f.next)
}
