package config

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// The real worker must replace an invalid WAL after storage repair.
func TestOAuthRound7_P1_1_InvalidWALWorkerRepair(t *testing.T) {
	f := newCodexRefreshFixture(t)
	journal := oauthJournalPath(f.path, f.used.ID)
	invalid := []byte("{invalid-active-wal")
	require.NoError(t, os.WriteFile(journal, invalid, 0o600))
	oldWrite, oldBudget := oauthJournalAtomicWrite, oauthJournalWriteBudget
	oldRename, oldCopy := oauthJournalQuarantineRename, oauthJournalQuarantineCopy
	oldBefore, oldWait := oauthJournalBeforeWorker, oauthJournalWorkerWait
	var failing atomic.Bool
	failing.Store(true)
	var writes, quarantines atomic.Int32
	oauthJournalWriteBudget = 10 * time.Millisecond
	oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		if path == journal {
			writes.Add(1)
			if failing.Load() {
				return errors.New("injected journal write failure")
			}
		}
		return oldWrite(path, data, mode)
	}
	oauthJournalQuarantineRename = func(string, string) error {
		quarantines.Add(1)
		return os.ErrPermission
	}
	oauthJournalQuarantineCopy = func(string, string) error { return os.ErrPermission }
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
	t.Cleanup(func() {
		f.store.stopOAuthJournalWorkers()
		oauthJournalAtomicWrite, oauthJournalWriteBudget = oldWrite, oldBudget
		oauthJournalQuarantineRename, oauthJournalQuarantineCopy = oldRename, oldCopy
		oauthJournalBeforeWorker, oauthJournalWorkerWait = oldBefore, oldWait
	})
	calls := 0
	stubCodexRefresh(t, func(_ context.Context, _ *http.Client, token string) (*oauth.Token, error) {
		calls++
		require.Equal(t, f.used.OAuthToken.RefreshToken, token)
		return f.next, nil
	})
	idle := hookOAuthJournalIdle(t, f.store)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, f.used, nil))
	require.Equal(t, 1, calls)
	require.Positive(t, writes.Load())
	require.Positive(t, quarantines.Load())
	entry := f.store.oauthOutboxCurrent(f.path, f.used.ID)
	require.NotNil(t, entry)
	require.Equal(t, f.next, entry.record.Token)
	actual, err := os.ReadFile(journal)
	require.NoError(t, err)
	require.Equal(t, invalid, actual)
	assertCodexRefreshTokens(t, f, f.next, f.used.OAuthToken)
	requireOAuthRound7Released(t, f)
	close(start)
	awaitCodexSignal(t, attempts)
	require.Same(t, entry, f.store.oauthOutboxCurrent(f.path, f.used.ID))
	failing.Store(false)
	// Three bounded real-worker attempts cover write, commit and removal.
	for range 3 {
		select {
		case permits <- struct{}{}:
		case <-time.After(time.Second):
			t.Fatal("worker did not accept repair permit")
		}
		select {
		case <-idle:
			f.store.stopOAuthJournalWorkers()
			assertCodexRefreshTokens(t, f, f.next, f.next)
			require.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
			_, err := os.Stat(journal)
			require.True(t, os.IsNotExist(err))
			return
		case <-attempts:
		case <-time.After(time.Second):
			t.Fatal("worker repair attempt did not finish")
		}
	}
	f.store.stopOAuthJournalWorkers()
	disk, err := readOAuthJournalDiskToken(f.path, f.used.ID)
	require.NoError(t, err)
	require.Equal(t, f.next, disk, "repaired worker must persist owned B despite invalid active WAL")
	t.Fatal("worker did not finish journal removal within three repair attempts")
}

// Equal config bytes cannot suppress a required exchange during sync uncertainty.
func TestOAuthRound7_P1_2_EqualConfigOutboxUncertainty(t *testing.T) {
	for _, name := range []string{"caller-used-B", "expired-B"} {
		t.Run(name, func(t *testing.T) {
			f := newOAuthJournalRefreshRecoveryFixture(t)
			b := *f.next
			if name == "expired-B" {
				b.ExpiresAt = time.Now().Add(-time.Minute).Unix()
			}
			d := b
			d.AccessToken, d.RefreshToken = "fixture-D-access", "fixture-D-refresh"
			d.ExpiresAt = time.Now().Add(2 * time.Hour).Unix()
			record := &oauthJournalRecord{PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(f.used.OAuthToken), Token: &b}
			entry := f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, record)
			require.NoError(t, f.store.applyOAuthTokenWithPending(f.used.ID, f.used.OAuthToken, &b, true))
			writeCodexPendingDisk(t, f, &b)
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			configTestHooks.Lock()
			oldSync := configTestHooks.syncParent
			configTestHooks.syncParent = func(string) error { return errors.New("injected parent sync failure") }
			configTestHooks.Unlock()
			oldBudget := oauthJournalWriteBudget
			oauthJournalWriteBudget = 10 * time.Millisecond
			t.Cleanup(func() {
				f.store.stopOAuthJournalWorkers()
				oauthJournalWriteBudget = oldBudget
				configTestHooks.Lock()
				configTestHooks.syncParent = oldSync
				configTestHooks.Unlock()
			})
			calls := 0
			client := &http.Client{}
			stubCodexRefresh(t, func(_ context.Context, actual *http.Client, token string) (*oauth.Token, error) {
				calls++
				require.Same(t, client, actual)
				require.Equal(t, b.RefreshToken, token)
				return &d, nil
			})
			used := f.used
			if name == "caller-used-B" {
				used.OAuthToken = &b
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err = f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, used, client)
			f.store.stopOAuthJournalWorkers()
			requireOAuthRound7Released(t, f)
			after, readErr := os.ReadFile(f.path)
			require.NoError(t, readErr)
			require.Equal(t, before, after, "uncertain config and unrelated data must remain intact")
			if calls == 0 {
				require.Same(t, entry, f.store.oauthOutboxCurrent(f.path, f.used.ID), "failed refresh must preserve owned B")
				assertCodexRefreshTokens(t, f, &b, &b)
			}
			require.Equal(t, 1, calls, "equal config with unverified durability must exchange B exactly once to D")
			require.NoError(t, err)
			assertCodexRefreshTokens(t, f, &d, &b)
			owned := f.store.oauthOutboxCurrent(f.path, f.used.ID)
			if owned != nil {
				require.Equal(t, &d, owned.record.Token)
				require.Equal(t, oauthRefreshTokenSHA256(&b), owned.record.PredecessorRefreshTokenSHA256)
			} else {
				wal, readErr := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
				require.NoError(t, readErr)
				require.NotNil(t, wal)
				require.Equal(t, &d, wal.Token)
				require.Equal(t, oauthRefreshTokenSHA256(&b), wal.PredecessorRefreshTokenSHA256)
			}
		})
	}
}

func requireOAuthRound7Released(t *testing.T, f codexRefreshFixture) {
	t.Helper()
	path := oauthRefreshLockPath(f.path, f.used.ID)
	lock, err := filelock.TryAcquireFileLock(path)
	require.NoError(t, err)
	require.NoError(t, releaseConfigFileLock(path, lock))
	codexRefreshActive.Lock()
	_, active := codexRefreshActive.calls[codexRefreshKey{f.store, f.used.ID}]
	codexRefreshActive.Unlock()
	require.False(t, active)
}
