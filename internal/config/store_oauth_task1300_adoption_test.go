package config

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestTask1300AdoptionLineage(t *testing.T) {
	for _, name := range []string{"digestless_older", "digestless_newer", "equal_external", "equal_owned_later", "equal_owned_tie", "burned_only"} {
		t.Run(name, func(t *testing.T) {
			f := task1292Fixture(t, task1292Provider)
			task1292PauseWorkers(t)
			task1292Descriptor(t, false, func(context.Context, *http.Client, string) (*oauth.Token, error) {
				t.Fatal("adoption must not exchange")
				return nil, nil
			})
			f.store.stopOAuthJournalWorkers()
			x := *f.next
			x.AccessToken, x.RefreshToken = "fixture-X-access", "fixture-X-refresh"
			x.ExpiresAt += 60
			live := x
			live.AccessToken = "fixture-live-access"
			live.ExpiresAt--
			external := &oauthJournalRecord{Token: &x}
			adopt := true
			switch name {
			case "digestless_newer":
				live.ExpiresAt = x.ExpiresAt + 1
				adopt = false
			case "equal_external":
				live = x
			case "equal_owned_later", "equal_owned_tie":
				live = *f.next
				external.PredecessorRefreshTokenSHA256 = oauthRefreshTokenSHA256(f.used.OAuthToken)
				if name == "equal_owned_tie" {
					x.ExpiresAt = live.ExpiresAt
				}
			case "burned_only":
				live.RefreshToken = "fixture-burned-refresh"
				live.ExpiresAt = x.ExpiresAt + 1
				external.PredecessorRefreshTokenSHA256 = oauthRefreshTokenSHA256(f.used.OAuthToken)
				external.BurnedRefreshTokenSHA256 = []string{oauthRefreshTokenSHA256(&live)}
				adopt = false
			}
			owned := &oauthJournalRecord{Token: f.next, PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(f.used.OAuthToken)}
			entry := f.store.installOAuthOutbox(f.path, ScopeGlobal, f.used.ID, owned)
			pc := f.used
			pc.OAuthToken, pc.APIKey = &live, ""
			f.store.SetProviderRuntimeConfig(pc.ID, pc)
			journal := oauthJournalPath(f.path, pc.ID)
			require.NoError(t, writeOAuthJournalOnce(journal, external))
			beforeDisk, err := os.ReadFile(f.path)
			require.NoError(t, err)
			beforeWAL, err := os.ReadFile(journal)
			require.NoError(t, err)
			err = f.store.flushOAuthOutbox(context.Background(), f.path, pc.ID)
			if adopt {
				require.NoError(t, err)
				require.Nil(t, f.store.oauthOutboxCurrent(f.path, pc.ID))
			} else {
				require.ErrorIs(t, err, errOAuthCredentialCAS)
				require.Same(t, entry, f.store.oauthOutboxCurrent(f.path, pc.ID))
				require.False(t, entry.obsolete)
			}
			pc, _ = f.store.Config().Providers.Get(pc.ID)
			f.store.publishMu.Lock()
			rotation, pending := f.store.pendingOAuth[pc.ID]
			f.store.publishMu.Unlock()
			require.Equal(t, adopt, pending)
			if adopt {
				require.Equal(t, &x, pc.OAuthToken)
				require.Equal(t, &x, &rotation.token)
				require.Equal(t, &live, &rotation.predecessor)
			} else {
				require.Equal(t, &live, pc.OAuthToken)
			}
			afterDisk, err := os.ReadFile(f.path)
			require.NoError(t, err)
			require.Equal(t, beforeDisk, afterDisk)
			afterWAL, err := os.ReadFile(journal)
			require.NoError(t, err)
			require.Equal(t, beforeWAL, afterWAL)
		})
	}
}

func TestTask1300OutboxExternalAdoption(t *testing.T) {
	for _, predecessor := range []bool{true, false} {
		name := "unrelated_newer_live"
		if predecessor {
			name = "true_predecessor_control"
		}
		t.Run(name, func(t *testing.T) {
			f := task1292Fixture(t, task1292Provider)
			task1292PauseWorkers(t)
			live := *f.next
			live.AccessToken = "fixture-Y-access"
			live.RefreshToken = "fixture-Y-refresh"
			live.ExpiresAt += 120
			if predecessor {
				live.AccessToken = "fixture-A2-access"
				live.RefreshToken = f.used.OAuthToken.RefreshToken
			}
			x := *f.next
			x.AccessToken = "fixture-X-access"
			x.RefreshToken = "fixture-X-refresh"
			x.ExpiresAt += 60
			require.Less(t, f.next.ExpiresAt, x.ExpiresAt)
			require.Less(t, x.ExpiresAt, live.ExpiresAt)

			journal := oauthJournalPath(f.path, f.used.ID)
			oldWrite, oldWait := oauthJournalAtomicWrite, oauthJournalRetryWait
			writes, waits := 0, 0
			oauthJournalAtomicWrite = func(path string, data []byte, mode os.FileMode) error {
				if path == journal {
					writes++
					return os.ErrPermission
				}
				return oldWrite(path, data, mode)
			}
			oauthJournalRetryWait = func(context.Context, time.Duration) error {
				waits++
				return context.DeadlineExceeded
			}
			t.Cleanup(func() {
				oauthJournalAtomicWrite, oauthJournalRetryWait = oldWrite, oldWait
			})
			joinOAuthJournalTestStoresBeforeRestore(t)

			calls := 0
			task1292Descriptor(t, false, func(_ context.Context, _ *http.Client, refresh string) (*oauth.Token, error) {
				calls++
				require.Equal(t, f.used.OAuthToken.RefreshToken, refresh)
				pc := f.used
				pc.OAuthToken, pc.APIKey = &live, ""
				f.store.SetProviderRuntimeConfig(pc.ID, pc)
				return f.next, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := f.store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, f.used, nil)
			if predecessor {
				require.ErrorContains(t, err, "credentials changed during OAuth refresh")
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, calls)
			require.Equal(t, 1, writes)
			require.Equal(t, 1, waits)
			f.store.stopOAuthJournalWorkers()
			oauthJournalAtomicWrite, oauthJournalRetryWait = oldWrite, oldWait

			entry := f.store.oauthOutboxCurrent(f.path, f.used.ID)
			require.NotNil(t, entry)
			require.Equal(t, f.next, entry.record.Token)
			require.False(t, entry.obsolete)
			require.Equal(t, predecessor, oauthJournalLivePredecessor(&entry.record, &live))
			f.store.publishMu.Lock()
			_, pending := f.store.pendingOAuth[f.used.ID]
			f.store.publishMu.Unlock()
			require.False(t, pending, "failed publication CAS must not install pending B")
			pc, _ := f.store.Config().Providers.Get(f.used.ID)
			require.Equal(t, &live, pc.OAuthToken)
			disk, err := readOAuthJournalDiskToken(f.path, f.used.ID)
			require.NoError(t, err)
			require.Equal(t, f.used.OAuthToken, disk)
			record, err := readOAuthJournal(journal)
			require.NoError(t, err)
			require.Nil(t, record)

			// External writer owns X; disk A still cannot settle owned B.
			external := &oauthJournalRecord{Token: &x, PredecessorRefreshTokenSHA256: oauthRefreshTokenSHA256(f.used.OAuthToken)}
			require.NoError(t, writeOAuthJournalOnce(journal, external))
			require.Equal(t, oauthJournalDiskLaterOrTie, arbitrateOAuthJournal(&entry.record, nil, &x, nil))
			settled, err := f.store.settleOAuthOutbox(ctx, f.path, f.used.ID, entry)
			require.NoError(t, err)
			require.False(t, settled)

			beforeDisk, err := os.ReadFile(f.path)
			require.NoError(t, err)
			beforeWAL, err := os.ReadFile(journal)
			require.NoError(t, err)
			done, attemptErr := f.store.oauthJournalWorkerAttempt(ctx, ScopeGlobal, f.used.ID, 0)
			require.False(t, done)
			if !predecessor {
				require.ErrorIs(t, attemptErr, errOAuthCredentialCAS)
				pc, _ = f.store.Config().Providers.Get(f.used.ID)
				require.Equal(t, &live, pc.OAuthToken)
				disk, err = readOAuthJournalDiskToken(f.path, f.used.ID)
				require.NoError(t, err)
				require.Equal(t, f.used.OAuthToken, disk)
				afterDisk, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.Equal(t, beforeDisk, afterDisk)
				afterWAL, err := os.ReadFile(journal)
				require.NoError(t, err)
				require.Equal(t, beforeWAL, afterWAL)
				require.Same(t, entry, f.store.oauthOutboxCurrent(f.path, f.used.ID))
				require.False(t, entry.obsolete)
				f.store.publishMu.Lock()
				_, pending = f.store.pendingOAuth[f.used.ID]
				f.store.publishMu.Unlock()
				require.False(t, pending, "rejected adoption must not synthesize pending X")
				return
			}
			require.NoError(t, attemptErr)
			pc, _ = f.store.Config().Providers.Get(f.used.ID)
			disk, err = readOAuthJournalDiskToken(f.path, f.used.ID)
			require.NoError(t, err)
			record, err = readOAuthJournal(journal)
			require.NoError(t, err)
			require.Equal(t, external, record, "external journal must be preserved")
			require.Nil(t, f.store.oauthOutboxCurrent(f.path, f.used.ID))
			f.store.publishMu.Lock()
			rotation, pending := f.store.pendingOAuth[f.used.ID]
			f.store.publishMu.Unlock()
			require.True(t, pending)
			require.Equal(t, &x, &rotation.token)
			require.Equal(t, &live, &rotation.predecessor)
			require.Empty(t, pc.APIKey)
			require.Equal(t, 1, calls)
			require.Equal(t, &x, pc.OAuthToken)
			require.Equal(t, &x, disk)
		})
	}
}
