package config

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	hyperp "github.com/PHPCraftdream/rush/internal/agent/hyper"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestTask1292Legacy_SlotWaitCancellationAndBudget(t *testing.T) {
	for _, id := range []string{hyperp.Name, string(catwalk.InferenceProviderCopilot)} {
		for _, bounded := range []bool{false, true} {
			name := id + "/cancel-waiter"
			if bounded {
				name = id + "/budget-waiter"
			}
			t.Run(name, func(t *testing.T) {
				f := task1292Fixture(t, id)
				entered, release := make(chan struct{}), make(chan struct{})
				leaderCtx, cancelLeader := context.WithCancel(context.Background())
				defer cancelLeader()
				var calls atomic.Int32
				task1292LegacySeam(t, id, func(context.Context, *http.Client, string) (*oauth.Token, error) {
					if calls.Add(1) != 1 {
						return nil, errors.New("waiter unexpectedly exchanged")
					}
					close(entered)
					select {
					case <-release:
						return f.next, nil
					case <-leaderCtx.Done():
						return nil, leaderCtx.Err()
					}
				})
				leader := task1292Start(t, f.store, f.used)
				awaitCodexSignal(t, entered)
				if bounded {
					old := codexRefreshLockWait
					codexRefreshLockWait = 50 * time.Millisecond
					t.Cleanup(func() { codexRefreshLockWait = old })
					joinOAuthJournalTestStoresBeforeRestore(t)
				}
				ctx, cancel := context.WithCancel(context.Background())
				waiting := &task1292WaitContext{Context: ctx, entered: make(chan struct{})}
				result, done := make(chan error, 1), make(chan struct{})
				go func() {
					defer close(done)
					result <- f.store.RefreshOAuthTokenWithClient(waiting, ScopeGlobal, f.used, nil)
				}()
				t.Cleanup(func() { cancel(); awaitCodexSignal(t, done) })
				joinOAuthJournalTestStoresBeforeRestore(t)
				awaitCodexSignal(t, waiting.entered)
				want := context.DeadlineExceeded
				if !bounded {
					cancel()
					want = context.Canceled
				}
				require.ErrorIs(t, awaitCodexResult(t, result), want)
				codexRefreshActive.Lock()
				_, busy := codexRefreshActive.calls[codexRefreshKey{f.store, id}]
				codexRefreshActive.Unlock()
				require.True(t, busy, "canceled waiter must not release leader's slot")
				close(release)
				require.NoError(t, awaitCodexResult(t, leader))
				require.Equal(t, int32(1), calls.Load())
				task1292LegacyReleased(t, f)
			})
		}
	}
}
