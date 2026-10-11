package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthRound9_RecoveredWALRetainsOriginalBurnedPredecessor(t *testing.T) {
	runOAuthRound9RecoveredPredecessor(t, 3600, true)
}

func TestOAuthRound9_RecoveredWALBurnedPredecessorCases(t *testing.T) {
	for _, delta := range []int64{-1800, 0, 3600} {
		for _, usedB := range []bool{false, true} {
			t.Run(fmt.Sprintf("AexpiryDelta=%d/usedB=%t", delta, usedB), func(t *testing.T) {
				runOAuthRound9RecoveredPredecessor(t, delta, usedB)
			})
		}
	}
}

func TestOAuthRound9_BurnedDigestNonfirstAuthority(t *testing.T) {
	for _, delta := range []int64{0, 3600} {
		t.Run(fmt.Sprintf("AexpiryDelta=%d", delta), func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			a, b := *f.used.OAuthToken, *f.next
			a.ExpiresAt = b.ExpiresAt + delta
			c, x := a, a
			c.RefreshToken, x.RefreshToken = "fixture-primary-C", "fixture-first-X"
			record := &oauthJournalRecord{
				Token: &b, PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(&c),
				BurnedRefreshTokenSHA256: []string{oauthRefreshTokenSHA256(&x), oauthRefreshTokenSHA256(&a)},
			}
			require.Equal(t, oauthJournalPredecessor, arbitrateOAuthJournal(record, &c, &a, nil), "nonfirst burned digest must beat expiry")
			f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, record)
			require.NoError(t, f.store.applyOAuthTokenWithPendingPredecessor(f.used.ID, f.used.OAuthToken, &b, &c, true, record))
			writeCodexRefreshDisk(t, f.path, &a)
			d := b
			d.AccessToken, d.RefreshToken = "fixture-D-access", "fixture-D-refresh"
			d.ExpiresAt = time.Now().Add(3 * time.Hour).Unix()
			oldWrite, oldBudget := oauthJournalAtomicWrite, oauthJournalWriteBudget
			journal := oauthJournalPath(f.path, f.used.ID)
			oauthJournalWriteBudget = 10 * time.Millisecond
			oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
				if path == journal {
					return os.ErrPermission
				}
				return oldWrite(path, data, mode)
			}
			t.Cleanup(func() {
				f.store.stopOAuthJournalWorkers()
				oauthJournalAtomicWrite, oauthJournalWriteBudget = oldWrite, oldBudget
			})
			calls := 0
			stubCodexRefresh(t, func(_ context.Context, _ *http.Client, refresh string) (*oauth.Token, error) {
				calls++
				assert.Equal(t, b.RefreshToken, refresh)
				pc, _ := f.store.Config().Providers.Get(f.used.ID)
				assert.Equal(t, &b, pc.OAuthToken, "nonfirst burned A must not publish")
				return &d, nil
			})
			used := f.used
			used.OAuthToken = &b
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			assert.NoError(t, f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, used, nil))
			assert.Equal(t, 1, calls)
			assertCodexRefreshTokens(t, f, &d, &a)
			requireOAuthRound7Released(t, f)
		})
	}
}

func runOAuthRound9RecoveredPredecessor(t *testing.T, delta int64, usedB bool) {
	t.Helper()
	f := newCodexRefreshFixture(t)
	a, b := *f.used.OAuthToken, *f.next
	a.ExpiresAt = b.ExpiresAt + delta
	c := b
	c.AccessToken, c.RefreshToken = "fixture-C-access", "fixture-C-refresh"
	c.ExpiresAt = b.ExpiresAt - 1800
	live := f.used
	live.OAuthToken = &c
	f.store.SetProviderRuntimeConfig(live.ID, live)
	writeCodexRefreshDisk(t, f.path, &c)
	journal := oauthJournalPath(f.path, f.used.ID)
	record := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(&a), Token: &b}
	walBytes, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(journal, walBytes, 0o600))

	oldBefore, oldWait := oauthJournalBeforeWorker, oauthJournalWorkerWait
	oldWrite, oldBudget := oauthJournalAtomicWrite, oauthJournalWriteBudget
	start, permits := make(chan struct{}), make(chan struct{})
	attempts := make(chan struct{}, 1)
	oauthJournalBeforeWorker = func(ctx context.Context) {
		select {
		case <-start:
		case <-ctx.Done():
		}
	}
	oauthJournalWorkerWait = func(ctx context.Context, _ time.Duration) error {
		select {
		case attempts <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-permits:
			return ctx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var failing atomic.Bool
	failing.Store(true)
	var writes atomic.Int32
	oauthJournalWriteBudget = 10 * time.Millisecond
	oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		if path == journal {
			writes.Add(1)
			if failing.Load() {
				return os.ErrPermission
			}
		}
		return oldWrite(path, data, mode)
	}
	t.Cleanup(func() {
		f.store.stopOAuthJournalWorkers()
		oauthJournalBeforeWorker, oauthJournalWorkerWait = oldBefore, oldWait
		oauthJournalAtomicWrite, oauthJournalWriteBudget = oldWrite, oldBudget
	})
	idle := hookOAuthJournalIdle(t, f.store)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lockPath := oauthRefreshLockPath(f.path, f.used.ID)
	lock, err := acquireConfigFileLock(ctx, lockPath)
	require.NoError(t, err)
	adopted, pendingRecord, recoveryErr := f.store.recoverCodexOAuthJournal(ctx, ScopeGlobal, f.used.ID)
	require.NoError(t, releaseConfigFileLock(lockPath, lock))
	require.NoError(t, recoveryErr)
	require.Equal(t, &b, adopted)
	require.Equal(t, record, pendingRecord)
	assertCodexRefreshTokens(t, f, &b, &c)
	f.store.publishMu.Lock()
	pending, pendingOK := f.store.pendingOAuth[f.used.ID]
	f.store.publishMu.Unlock()
	require.True(t, pendingOK)
	require.Equal(t, c, pending.predecessor)
	require.Equal(t, b, pending.token)
	require.Equal(t, oauthRefreshTokenSHA256(&a), pending.record.PredecessorRefreshTokenSHA256)
	require.Contains(t, pending.record.BurnedRefreshTokenSHA256, oauthRefreshTokenSHA256(&c))
	retained, err := os.ReadFile(journal)
	require.NoError(t, err)
	require.Equal(t, walBytes, retained)
	f.store.startOAuthJournalWorker(ScopeGlobal, f.used.ID)

	writeCodexRefreshDisk(t, f.path, &a)
	d := b
	d.AccessToken, d.RefreshToken = "fixture-D-access", "fixture-D-refresh"
	d.ExpiresAt = time.Now().Add(3 * time.Hour).Unix()
	calls := 0
	client := &http.Client{}
	stubCodexRefresh(t, func(_ context.Context, actual *http.Client, refresh string) (*oauth.Token, error) {
		calls++
		assert.Same(t, client, actual)
		assert.Equal(t, b.RefreshToken, refresh)
		pc, ok := f.store.Config().Providers.Get(f.used.ID)
		assert.True(t, ok)
		assert.Equal(t, &b, pc.OAuthToken, "original burned A must never publish")
		return &d, nil
	})
	used := f.used
	used.OAuthToken = &c
	want, wantCalls := &b, 0
	if usedB {
		used.OAuthToken = &b
		want, wantCalls = &d, 1
	}
	err = f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, used, client)
	assert.NoError(t, err)
	assert.Equal(t, wantCalls, calls, "only caller-used B requires exchange to D")
	pc, ok := f.store.Config().Providers.Get(f.used.ID)
	assert.True(t, ok)
	assert.Equal(t, want, pc.OAuthToken)
	requireOAuthRound7Released(t, f)
	if calls != wantCalls {
		return
	}
	if usedB {
		require.Positive(t, writes.Load())
		owned := f.store.oauthOutboxCurrent(f.path, f.used.ID)
		require.NotNil(t, owned)
		require.Equal(t, &d, owned.record.Token)
		require.Equal(t, oauthRefreshTokenSHA256(&b), owned.record.PredecessorRefreshTokenSHA256)
		require.Contains(t, owned.record.BurnedRefreshTokenSHA256, oauthRefreshTokenSHA256(&a))
		require.Contains(t, owned.record.BurnedRefreshTokenSHA256, oauthRefreshTokenSHA256(&c))
	}
	assertCodexRefreshTokens(t, f, want, &a)
	failing.Store(false)
	close(start)
	for range 6 {
		select {
		case <-idle:
			f.store.stopOAuthJournalWorkers()
			assertCodexRefreshTokens(t, f, want, want)
			require.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
			f.store.publishMu.Lock()
			_, pendingOK := f.store.pendingOAuth[f.used.ID]
			f.store.publishMu.Unlock()
			require.False(t, pendingOK, "convergence must clear pending ownership")
			_, err := os.Stat(journal)
			require.True(t, os.IsNotExist(err))
			requireOAuthRound7Released(t, f)
			return
		case <-attempts:
			select {
			case permits <- struct{}{}:
			case <-ctx.Done():
				t.Fatal("worker did not accept repair permit")
			}
		case <-ctx.Done():
			t.Fatal("worker did not converge after repair")
		}
	}
	t.Fatal("worker exceeded six repair attempts")
}
