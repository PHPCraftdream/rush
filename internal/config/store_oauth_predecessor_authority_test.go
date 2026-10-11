package config

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthRound9_OwnedBurnedPredecessorEqualExpiry(t *testing.T) {
	runOAuthRound9BurnedPredecessor(t, 0)
}

func TestOAuthRound9_OwnedBurnedPredecessorLaterExpiry(t *testing.T) {
	runOAuthRound9BurnedPredecessor(t, 3600)
}

// A is still valid by time, but its refresh credential was consumed to issue B.
func runOAuthRound9BurnedPredecessor(t *testing.T, expiryDelta int64) {
	t.Helper()
	f := newOAuthJournalRefreshRecoveryFixture(t)
	a, b := *f.used.OAuthToken, *f.next
	a.ExpiresAt = b.ExpiresAt + expiryDelta
	f.used.OAuthToken = &a
	f.store.SetProviderRuntimeConfig(f.used.ID, f.used)
	writeCodexPendingDisk(t, f, &a)
	record := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(&a), Token: &b}
	f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, record)
	require.NoError(t, f.store.applyOAuthTokenWithPending(f.used.ID, &a, &b, true))
	before, err := os.ReadFile(f.path)
	require.NoError(t, err)
	d := b
	d.AccessToken, d.RefreshToken = "round9-D-access", "round9-D-refresh"
	d.ExpiresAt = time.Now().Add(3 * time.Hour).Unix()

	journal := oauthJournalPath(f.path, f.used.ID)
	oldWrite, oldBudget := oauthJournalAtomicWrite, oauthJournalWriteBudget
	oauthJournalWriteBudget = 10 * time.Millisecond
	writes := 0
	oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		if path == journal {
			writes++
			return errors.New("injected round9 journal write failure")
		}
		return oldWrite(path, data, mode)
	}
	t.Cleanup(func() {
		f.store.stopOAuthJournalWorkers()
		oauthJournalAtomicWrite, oauthJournalWriteBudget = oldWrite, oldBudget
	})
	calls := 0
	client := &http.Client{}
	stubCodexRefresh(t, func(_ context.Context, actual *http.Client, refresh string) (*oauth.Token, error) {
		calls++
		assert.Same(t, client, actual)
		assert.Equal(t, b.RefreshToken, refresh, "exchange must consume B, never burned A")
		live, ok := f.store.Config().Providers.Get(f.used.ID)
		assert.True(t, ok)
		assert.Equal(t, &b, live.OAuthToken, "disk A must never replace owned memory B")
		return &d, nil
	})
	used := f.used
	used.OAuthToken = &b
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, used, client)
	f.store.stopOAuthJournalWorkers()
	requireOAuthRound7Released(t, f)
	target, targetErr := f.store.resolveConfigWriteTarget(f.path)
	require.NoError(t, targetErr)
	lock, lockErr := filelock.TryAcquireFileLock(target.lockPath)
	require.NoError(t, lockErr)
	require.NoError(t, releaseConfigFileLock(target.lockPath, lock))
	assert.NoError(t, err)
	assert.Equal(t, 1, calls, "owned B must exchange exactly once to D")
	assert.Positive(t, writes, "D must attempt journal handoff")
	live, ok := f.store.Config().Providers.Get(f.used.ID)
	assert.True(t, ok)
	assert.Equal(t, &d, live.OAuthToken)
	assert.Empty(t, live.APIKey)
	owned := f.store.oauthOutboxCurrent(f.path, f.used.ID)
	if assert.NotNil(t, owned, "failed journal writes must retain D ownership") {
		assert.Equal(t, &d, owned.record.Token)
		assert.Equal(t, oauthRefreshTokenSHA256(&b), owned.record.PredecessorRefreshTokenSHA256)
	}
	f.store.publishMu.Lock()
	pending, pendingOK := f.store.pendingOAuth[f.used.ID]
	f.store.publishMu.Unlock()
	assert.True(t, pendingOK)
	assert.Equal(t, d, pending.token)
	assert.Equal(t, b, pending.predecessor)
	after, readErr := os.ReadFile(f.path)
	require.NoError(t, readErr)
	assert.Equal(t, before, after, "blocked worker must leave config A untouched")
	if assert.True(t, f.store.diskWriteMu.TryLock(), "config mutex must be released") {
		f.store.diskWriteMu.Unlock()
	}
}
