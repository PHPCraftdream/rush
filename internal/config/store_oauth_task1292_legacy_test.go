package config

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	hyperp "github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

type task1292WaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (ctx *task1292WaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.entered) })
	return ctx.Context.Done()
}

func task1292LegacySeam(t *testing.T, id string, fn func(context.Context, *http.Client, string) (*oauth.Token, error)) {
	t.Helper()
	if id == hyperp.Name {
		old := hyperExchangeTokenFn
		hyperExchangeTokenFn = fn
		t.Cleanup(func() { hyperExchangeTokenFn = old })
	} else {
		old := copilotRefreshTokenFn
		copilotRefreshTokenFn = fn
		t.Cleanup(func() { copilotRefreshTokenFn = old })
	}
	joinOAuthJournalTestStoresBeforeRestore(t)
}

func task1292LegacyReleased(t *testing.T, f codexRefreshFixture) {
	t.Helper()
	codexRefreshActive.Lock()
	_, busy := codexRefreshActive.calls[codexRefreshKey{f.store, f.used.ID}]
	codexRefreshActive.Unlock()
	require.False(t, busy, "legacy slot must be released")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	path := oauthRefreshLockPath(f.path, f.used.ID)
	lock, err := acquireConfigFileLock(ctx, path)
	require.NoError(t, err, "legacy refresh sidecar must be released")
	require.NoError(t, releaseConfigFileLock(path, lock))
}

func TestTask1292Legacy_SingleExchangeWinnerAdoption(t *testing.T) {
	for _, id := range []string{hyperp.Name, string(catwalk.InferenceProviderCopilot)} {
		for _, secondStore := range []bool{false, true} {
			name := id + "/same-store"
			if secondStore {
				name = id + "/second-store"
			}
			t.Run(name, func(t *testing.T) {
				f := task1292Fixture(t, id)
				s := f.store
				if secondStore {
					s = task1292Store(t, f.path, f.used)
				}
				entered, release := make(chan struct{}), make(chan struct{})
				duplicate := make(chan struct{}, 1)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var calls atomic.Int32
				task1292LegacySeam(t, id, func(context.Context, *http.Client, string) (*oauth.Token, error) {
					if calls.Add(1) != 1 {
						duplicate <- struct{}{}
						return nil, errors.New("duplicate legacy exchange")
					}
					close(entered)
					select {
					case <-release:
						return f.next, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				})
				first := task1292Start(t, f.store, f.used)
				awaitCodexSignal(t, entered)
				secondEntered := make(chan struct{})
				if secondStore {
					lockPath := oauthRefreshLockPath(f.path, id)
					var signalOnce sync.Once
					hookCodexRefreshLock(t, func(ctx context.Context, path string) (*filelock.FileLock, error) {
						if path == lockPath {
							signalOnce.Do(func() { close(secondEntered) })
						}
						return filelock.AcquireFileLockContext(ctx, path)
					})
				}
				secondContext := context.Context(ctx)
				if !secondStore {
					secondContext = &task1292WaitContext{Context: ctx, entered: secondEntered}
				}
				secondStarted := make(chan struct{})
				secondResult := make(chan error, 1)
				secondDone := make(chan struct{})
				go func() {
					defer close(secondDone)
					close(secondStarted)
					secondResult <- s.RefreshOAuthTokenWithClient(secondContext, ScopeGlobal, f.used, nil)
				}()
				t.Cleanup(func() { cancel(); awaitCodexSignal(t, secondDone) })
				joinOAuthJournalTestStoresBeforeRestore(t)
				awaitCodexSignal(t, secondStarted)
				select {
				case <-secondEntered:
				case <-duplicate:
				case <-time.After(5 * time.Second):
					t.Error("second caller neither waited nor exchanged")
					cancel()
				}
				close(release)
				firstErr := awaitCodexResult(t, first)
				secondErr := awaitCodexResult(t, secondResult)
				awaitCodexSignal(t, secondDone)
				require.Equal(t, int32(1), calls.Load(), "serialization must prevent duplicate exchange")
				require.NoError(t, firstErr)
				require.NoError(t, secondErr)
				for _, store := range []*ConfigStore{f.store, s} {
					pc, _ := store.Config().Providers.Get(id)
					require.Equal(t, f.next, pc.OAuthToken)
					require.Equal(t, f.next.AccessToken, pc.APIKey)
				}
				task1292LegacyReleased(t, f)
			})
		}
	}
}

func TestTask1292Legacy_EmptyRefreshUnchangedSemantics(t *testing.T) {
	for _, id := range []string{hyperp.Name, string(catwalk.InferenceProviderCopilot)} {
		t.Run(id, func(t *testing.T) {
			f := task1292Fixture(t, id)
			b := *f.next
			b.RefreshToken = ""
			task1292LegacySeam(t, id, func(context.Context, *http.Client, string) (*oauth.Token, error) { return &b, nil })
			require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
			pc, _ := f.store.Config().Providers.Get(id)
			require.Empty(t, pc.OAuthToken.RefreshToken)
			require.Empty(t, pc.OAuthToken.AccountID)
			require.Equal(t, b.AccessToken, pc.APIKey)
			disk, err := readOAuthJournalDiskToken(f.path, id)
			require.NoError(t, err)
			require.Equal(t, &b, disk)
			task1292LegacyReleased(t, f)
		})
	}
}

func TestTask1292Legacy_ErrorAndCancelRelease(t *testing.T) {
	for _, id := range []string{hyperp.Name, string(catwalk.InferenceProviderCopilot)} {
		for _, canceled := range []bool{false, true} {
			name := id + "/error"
			if canceled {
				name = id + "/cancel"
			}
			t.Run(name, func(t *testing.T) {
				f := task1292Fixture(t, id)
				failure := errors.New("fixture exchange failure")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				entered := make(chan struct{})
				task1292LegacySeam(t, id, func(exchangeCtx context.Context, _ *http.Client, _ string) (*oauth.Token, error) {
					if !canceled {
						return nil, failure
					}
					close(entered)
					<-exchangeCtx.Done()
					return nil, exchangeCtx.Err()
				})
				result := make(chan error, 1)
				done := make(chan struct{})
				go func() {
					defer close(done)
					result <- f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, f.used, nil)
				}()
				t.Cleanup(func() { cancel(); awaitCodexSignal(t, done) })
				joinOAuthJournalTestStoresBeforeRestore(t)
				if canceled {
					awaitCodexSignal(t, entered)
					cancel()
					failure = context.Canceled
				}
				require.ErrorIs(t, awaitCodexResult(t, result), failure)
				task1292LegacyReleased(t, f)
			})
		}
	}
}

func TestTask1292Legacy_HeldSidecarBoundedWait(t *testing.T) {
	for _, id := range []string{hyperp.Name, string(catwalk.InferenceProviderCopilot)} {
		t.Run(id, func(t *testing.T) {
			f := task1292Fixture(t, id)
			path := oauthRefreshLockPath(f.path, id)
			lock, err := filelock.AcquireFileLockContext(context.Background(), path)
			require.NoError(t, err)
			released := false
			t.Cleanup(func() {
				if !released {
					_ = lock.Release()
				}
			})
			old := codexRefreshLockWait
			codexRefreshLockWait = 50 * time.Millisecond
			t.Cleanup(func() { codexRefreshLockWait = old })
			joinOAuthJournalTestStoresBeforeRestore(t)
			var calls atomic.Int32
			task1292LegacySeam(t, id, func(context.Context, *http.Client, string) (*oauth.Token, error) {
				calls.Add(1)
				return f.next, nil
			})
			err = f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil)
			require.ErrorContains(t, err, "lock OAuth refresh")
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Zero(t, calls.Load())
			require.NoError(t, lock.Release())
			released = true
			task1292LegacyReleased(t, f)
		})
	}
}
