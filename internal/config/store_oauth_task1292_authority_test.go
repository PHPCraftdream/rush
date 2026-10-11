package config

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestTask1292Engine_SameRefreshExpiryAuthority(t *testing.T) {
	for _, delta := range []int64{-60, 0, 60} {
		for _, route := range []string{"pending", "outbox", "recovery"} {
			name := route + "/earlier"
			if delta == 0 {
				name = route + "/equal"
			} else if delta > 0 {
				name = route + "/later"
			}
			t.Run(name, func(t *testing.T) {
				f := task1292Fixture(t, task1292Provider)
				task1292PauseWorkers(t)
				task1292Descriptor(t, false, func(context.Context, *http.Client, string) (*oauth.Token, error) {
					t.Error("authority arbitration must not exchange")
					return nil, errors.New("unexpected exchange")
				})
				a, b := *f.used.OAuthToken, *f.next
				b.RefreshToken = a.RefreshToken
				a.ExpiresAt = b.ExpiresAt + delta
				f.used.OAuthToken = &a
				f.used.APIKey = a.AccessToken
				f.store.SetProviderRuntimeConfig(f.used.ID, f.used)
				task1292WriteDisk(t, f.path, f.used.ID, &a, a.AccessToken)
				record := &oauthJournalRecord{Token: &b}
				require.NoError(t, f.store.applyOAuthTokenWithPendingPredecessor(f.used.ID, &a, &b, &a, true, record))
				want := &b
				if delta >= 0 {
					want = &a
				}
				switch route {
				case "pending":
					f.store.publishMu.Lock()
					candidate := f.store.loadSnapshot().clone()
					candidate.config = &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{f.used.ID: f.used})}
					f.store.overlayPendingOAuthLocked(candidate)
					_, pending := f.store.pendingOAuth[f.used.ID]
					f.store.publishMu.Unlock()
					pc, _ := candidate.config.Providers.Get(f.used.ID)
					require.Equal(t, want, pc.OAuthToken)
					require.Equal(t, delta < 0, pending)
				case "outbox":
					entry := f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, record)
					settled, err := f.store.settleOAuthOutbox(context.Background(), f.path, f.used.ID, entry)
					require.NoError(t, err)
					require.Equal(t, delta >= 0, settled)
					pc, _ := f.store.Config().Providers.Get(f.used.ID)
					require.Equal(t, want, pc.OAuthToken)
					require.Empty(t, entry.record.PredecessorRefreshTokenSHA256)
					require.Empty(t, entry.record.BurnedRefreshTokenSHA256)
				case "recovery":
					require.NoError(t, writeOAuthJournalOnce(oauthJournalPath(f.path, f.used.ID), record))
					adopted, remaining, err := task1292Recover(t, f)
					require.NoError(t, err)
					require.Equal(t, want, adopted)
					if delta < 0 {
						require.NotNil(t, remaining)
					} else {
						require.Nil(t, remaining)
					}
					pc, _ := f.store.Config().Providers.Get(f.used.ID)
					require.Equal(t, want, pc.OAuthToken)
				}
			})
		}
	}
}

func TestTask1292Engine_LogoutClearsOwnership(t *testing.T) {
	f := task1292Fixture(t, task1292Provider)
	task1292PauseWorkers(t)
	calls := 0
	task1292Descriptor(t, false, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		calls++
		return f.next, nil
	})
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	task1292WriteDisk(t, f.path, f.used.ID, nil, "")
	adopted, record, err := task1292Recover(t, f)
	require.NoError(t, err)
	require.Nil(t, adopted)
	require.Nil(t, record)
	pc, _ := f.store.Config().Providers.Get(f.used.ID)
	require.Nil(t, pc.OAuthToken)
	require.Empty(t, pc.APIKey)
	f.store.publishMu.Lock()
	_, pending := f.store.pendingOAuth[f.used.ID]
	f.store.publishMu.Unlock()
	require.False(t, pending)
	require.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
	require.Equal(t, 1, calls)
}

func TestTask1292Engine_OptionalDigestValidationAndHistoryBuilders(t *testing.T) {
	f := task1292Fixture(t, task1292Provider)
	task1292Descriptor(t, false, nil)
	a, b, c := *f.used.OAuthToken, *f.next, *f.next
	c.RefreshToken = "fixture-C-refresh"
	for _, primary := range []string{"", oauthRefreshTokenSHA256(&a), "bad", "zz"} {
		record := &oauthJournalRecord{Token: &b, PredecessorRefreshTokenSHA256: primary}
		require.Equal(t, primary == "" || primary == oauthRefreshTokenSHA256(&a), validOAuthJournal(record))
	}
	require.False(t, validOAuthJournal(&oauthJournalRecord{Token: &b, BurnedRefreshTokenSHA256: []string{"bad"}}))
	require.NoError(t, writeOAuthJournalOnce(oauthJournalPath(f.path, f.used.ID), &oauthJournalRecord{Token: &b}))
	read, err := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
	require.NoError(t, err)
	require.Empty(t, read.PredecessorRefreshTokenSHA256)
	history := &oauthJournalRecord{Token: &b, PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(&a), BurnedRefreshTokenSHA256: []string{oauthRefreshTokenSHA256(&c), oauthRefreshTokenSHA256(&c)}}
	entry := f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, history)
	require.Equal(t, []string{oauthRefreshTokenSHA256(&c)}, entry.record.BurnedRefreshTokenSHA256)
	require.NoError(t, f.store.applyOAuthTokenWithPendingPredecessor(f.used.ID, &a, &b, &a, true, history))
	next := b
	next.AccessToken = "fixture-B2-access"
	next.ExpiresAt += 60
	require.NoError(t, f.store.applyOAuthTokenWithPending(f.used.ID, &b, &next, true))
	f.store.publishMu.Lock()
	pending := f.store.pendingOAuth[f.used.ID]
	f.store.publishMu.Unlock()
	require.Empty(t, pending.record.PredecessorRefreshTokenSHA256)
	require.ElementsMatch(t, []string{oauthRefreshTokenSHA256(&a), oauthRefreshTokenSHA256(&c)}, pending.record.BurnedRefreshTokenSHA256)
	require.NotContains(t, pending.record.BurnedRefreshTokenSHA256, oauthRefreshTokenSHA256(&b))
	owned := f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, &oauthJournalRecord{Token: &next, PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(&b)})
	require.Empty(t, owned.record.PredecessorRefreshTokenSHA256)
	require.ElementsMatch(t, pending.record.BurnedRefreshTokenSHA256, owned.record.BurnedRefreshTokenSHA256)
}

func TestTask1292Engine_JournalFailureRetainsPendingOutbox(t *testing.T) {
	f := task1292Fixture(t, task1292Provider)
	task1292PauseWorkers(t)
	oldWrite, oldBudget := oauthJournalAtomicWrite, oauthJournalWriteBudget
	journal := oauthJournalPath(f.path, f.used.ID)
	oauthJournalWriteBudget = time.Second
	oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
		if path == journal {
			return os.ErrPermission
		}
		return oldWrite(path, data, mode)
	}
	oldWait := oauthJournalRetryWait
	oauthJournalRetryWait = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	t.Cleanup(func() {
		oauthJournalAtomicWrite, oauthJournalWriteBudget, oauthJournalRetryWait = oldWrite, oldBudget, oldWait
	})
	joinOAuthJournalTestStoresBeforeRestore(t)
	b := *f.next
	b.RefreshToken = f.used.OAuthToken.RefreshToken
	task1292Descriptor(t, false, func(context.Context, *http.Client, string) (*oauth.Token, error) { return &b, nil })
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	entry := f.store.oauthOutboxCurrent(f.path, f.used.ID)
	require.NotNil(t, entry)
	require.Empty(t, entry.record.PredecessorRefreshTokenSHA256)
	pc, _ := f.store.Config().Providers.Get(f.used.ID)
	require.Equal(t, &b, pc.OAuthToken)
	f.store.publishMu.Lock()
	pending := f.store.pendingOAuth[f.used.ID]
	f.store.publishMu.Unlock()
	require.Equal(t, &b, pending.record.Token)
	// Repair and hand off to a fresh store without invoking external discovery.
	oauthJournalAtomicWrite = oldWrite
	lockPath := oauthRefreshLockPath(f.path, f.used.ID)
	lock, err := acquireConfigFileLock(context.Background(), lockPath)
	require.NoError(t, err)
	require.NoError(t, f.store.flushOAuthOutbox(context.Background(), f.path, f.used.ID))
	require.NoError(t, releaseConfigFileLock(lockPath, lock))
	data, err := os.ReadFile(journal)
	require.NoError(t, err)
	var record oauthJournalRecord
	require.NoError(t, json.Unmarshal(data, &record))
	require.Equal(t, &b, record.Token)
}
