package config

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func task1296Fixture(t *testing.T, delta int64) (codexRefreshFixture, *oauth.Token, *oauthJournalRecord) {
	t.Helper()
	f := task1292Fixture(t, task1292Provider)
	task1292PauseWorkers(t)
	task1292Descriptor(t, false, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		t.Error("digestless authority must not exchange")
		return nil, errors.New("unexpected exchange")
	})
	f.next.RefreshToken = f.used.OAuthToken.RefreshToken
	c := *f.next
	c.AccessToken = "fixture-C-access"
	c.ExpiresAt += delta
	record := &oauthJournalRecord{Token: f.next}
	require.NoError(t, f.store.applyOAuthTokenWithPendingPredecessor(f.used.ID, f.used.OAuthToken, f.next, f.used.OAuthToken, true, record))
	pc := f.used
	pc.OAuthToken, pc.APIKey = &c, ""
	f.store.SetProviderRuntimeConfig(pc.ID, pc)
	return f, &c, record
}

func task1296State(t *testing.T, f codexRefreshFixture, want *oauth.Token, before []byte) {
	t.Helper()
	pc, ok := f.store.Config().Providers.Get(f.used.ID)
	assert.True(t, ok)
	assert.Equal(t, want, pc.OAuthToken)
	assert.Empty(t, pc.APIKey)
	after, err := os.ReadFile(f.path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "config bytes must remain unchanged")
}

func TestTask1296DigestlessArbiterAndBurnedSuccessor(t *testing.T) {
	for _, delta := range []int64{-60, 0, 60} {
		t.Run(fmt.Sprintf("delta_%+d", delta), func(t *testing.T) {
			f, c, record := task1296Fixture(t, delta)
			want := oauthJournalLater
			if delta >= 0 {
				want = oauthJournalDiskLaterOrTie
			}
			assert.Equal(t, want, arbitrateOAuthJournal(record, nil, c, nil))
			filtered := oauthJournalWithBurned(record, oauthRefreshTokenSHA256(f.next))
			assert.Empty(t, filtered.PredecessorRefreshTokenSHA256)
			assert.Empty(t, filtered.BurnedRefreshTokenSHA256)
			assert.Equal(t, want, arbitrateOAuthJournal(&filtered, nil, c, nil))
		})
	}
}

func TestTask1296DigestlessRoutes(t *testing.T) {
	for _, delta := range []int64{-60, 0, 60} {
		for _, route := range []string{"recovery_journal", "recovery_outbox", "settle", "worker_journal"} {
			t.Run(fmt.Sprintf("%s/delta_%+d", route, delta), func(t *testing.T) {
				f, c, record := task1296Fixture(t, delta)
				journal := oauthJournalPath(f.path, f.used.ID)
				require.NoError(t, writeOAuthJournalOnce(journal, record))
				var entry *oauthOutboxEntry
				if route == "recovery_outbox" || route == "settle" {
					entry = f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, record)
				}
				if route == "settle" {
					task1292WriteDisk(t, f.path, f.used.ID, f.next, "")
				}
				before, err := os.ReadFile(f.path)
				require.NoError(t, err)
				want := f.next
				if delta >= 0 {
					want = c
				}
				switch route {
				case "recovery_journal", "recovery_outbox":
					adopted, remaining, err := task1292Recover(t, f)
					if delta >= 0 {
						assert.ErrorIs(t, err, errOAuthCredentialCAS)
						assert.Nil(t, adopted)
					} else {
						assert.NoError(t, err)
						assert.Equal(t, f.next, adopted)
					}
					assert.Equal(t, record, remaining)
				case "settle":
					settled, err := f.store.settleOAuthOutbox(context.Background(), f.path, f.used.ID, entry)
					assert.NoError(t, err)
					assert.True(t, settled)
					assert.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
					f.store.publishMu.Lock()
					_, pending := f.store.pendingOAuth[f.used.ID]
					f.store.publishMu.Unlock()
					assert.False(t, pending)
				case "worker_journal":
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					done, err := f.store.oauthJournalWorkerAttempt(ctx, ScopeGlobal, f.used.ID, 0)
					assert.False(t, done)
					if delta >= 0 {
						assert.ErrorIs(t, err, errOAuthCredentialCAS)
					} else {
						assert.NoError(t, err)
						disk, readErr := readOAuthJournalDiskToken(f.path, f.used.ID)
						assert.NoError(t, readErr)
						assert.Equal(t, f.next, disk)
						before, err = os.ReadFile(f.path)
						require.NoError(t, err)
					}
				}
				task1296State(t, f, want, before)
				persisted, err := readOAuthJournal(journal)
				require.NoError(t, err)
				assert.Equal(t, record, persisted, "journal B must remain")
				if route == "recovery_outbox" {
					assert.Same(t, entry, f.store.oauthOutboxCurrent(f.path, f.used.ID))
				}
			})
		}
	}
}

func TestTask1296DigestlessCallers(t *testing.T) {
	for _, caller := range []string{"A", "C"} {
		t.Run(caller, func(t *testing.T) {
			f, c, record := task1296Fixture(t, 60)
			require.NoError(t, writeOAuthJournalOnce(oauthJournalPath(f.path, f.used.ID), record))
			before, err := os.ReadFile(f.path)
			require.NoError(t, err)
			pc := f.used
			if caller == "C" {
				pc.OAuthToken, pc.APIKey = c, ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, pc, nil)
			if caller == "A" {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, errOAuthCredentialCAS)
			}
			task1296State(t, f, c, before)
			persisted, err := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
			require.NoError(t, err)
			assert.Equal(t, record, persisted)
		})
	}
}

func TestTask1296ErrorBranchEqualLiveHasNoPredecessor(t *testing.T) {
	for _, rotated := range []bool{false, true} {
		t.Run(fmt.Sprintf("rotated_%t", rotated), func(t *testing.T) {
			f, _, record := task1296Fixture(t, 0)
			if rotated {
				f.next.RefreshToken = "fixture-rotated-refresh"
				record.PredecessorRefreshTokenSHA256 = oauthRefreshTokenSHA256(f.used.OAuthToken)
			}
			pc := f.used
			pc.OAuthToken, pc.APIKey = f.next, ""
			f.store.SetProviderRuntimeConfig(pc.ID, pc)
			f.store.publishMu.Lock()
			delete(f.store.pendingOAuth, pc.ID)
			f.store.publishMu.Unlock()
			require.NoError(t, os.WriteFile(f.path, []byte("{"), 0o600))
			require.NoError(t, writeOAuthJournalOnce(oauthJournalPath(f.path, pc.ID), record))
			adopted, remaining, err := task1292Recover(t, f)
			assert.ErrorIs(t, err, errOAuthJournalDisk)
			assert.Equal(t, f.next, adopted)
			assert.Equal(t, record, remaining)
			f.store.publishMu.Lock()
			_, pending := f.store.pendingOAuth[pc.ID]
			f.store.publishMu.Unlock()
			assert.False(t, pending, "equal live B must not become its own predecessor")
		})
	}
}
