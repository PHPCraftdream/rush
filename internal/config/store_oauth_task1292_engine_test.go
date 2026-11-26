package config

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthcred"
	"github.com/PHPCraftdream/rush/internal/config/store/oauthjournal"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"

	"github.com/stretchr/testify/require"
)

const task1292Provider = "fixture-task1292-rotating"

func task1292Descriptor(t *testing.T, preserve bool, exchange func(context.Context, *http.Client, string) (*oauth.Token, error)) {
	t.Helper()
	base := oauthProviderDescriptors()
	old := *oauthProviderDescriptorsOverride
	*oauthProviderDescriptorsOverride = func() []oauthProviderDescriptor {
		return append(append([]oauthProviderDescriptor(nil), base...), oauthcred.NewRotatingDescriptorForTest(task1292Provider, preserve, exchange))
	}
	t.Cleanup(func() { *oauthProviderDescriptorsOverride = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
}

func task1292WriteDisk(t *testing.T, path, id string, token *oauth.Token, apiKey string) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"providers": map[string]any{id: map[string]any{"oauth": token, "api_key": apiKey}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func task1292Store(t *testing.T, path string, pc ProviderConfig) *ConfigStore {
	t.Helper()
	s := newTestConfigStore(testStoreOpts{
		config:         &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{pc.ID: pc})},
		globalDataPath: path,
	})
	trackOAuthJournalTestStore(t, s)
	return s
}

func task1292Fixture(t *testing.T, id string) codexRefreshFixture {
	t.Helper()
	a := &oauth.Token{AccessToken: "fixture-A-access", RefreshToken: "fixture-A-refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), AccountID: "fixture-account"}
	b := &oauth.Token{AccessToken: "fixture-B-access", RefreshToken: "fixture-B-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	pc := ProviderConfig{ID: id, OAuthToken: a, APIKey: a.AccessToken}
	path := filepath.Join(t.TempDir(), "rush.json")
	task1292WriteDisk(t, path, id, a, pc.APIKey)
	return codexRefreshFixture{store: task1292Store(t, path, pc), path: path, used: pc, next: b}
}

func task1292PauseWorkers(t *testing.T) {
	t.Helper()
	old := oauthjournal.OAuthJournalBeforeWorker
	oauthjournal.OAuthJournalBeforeWorker = func(ctx context.Context) { <-ctx.Done() }
	t.Cleanup(func() { oauthjournal.OAuthJournalBeforeWorker = old })
	joinOAuthJournalTestStoresBeforeRestore(t)
}

func task1292Start(t *testing.T, s *ConfigStore, pc ProviderConfig) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- s.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, pc, nil)
	}()
	t.Cleanup(func() {
		cancel()
		awaitCodexSignal(t, done)
	})
	joinOAuthJournalTestStoresBeforeRestore(t)
	return result
}

func task1292Recover(t *testing.T, f codexRefreshFixture) (*oauth.Token, *oauthJournalRecord, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := oauthRefreshLockPath(f.path, f.used.ID)
	lock, err := acquireConfigFileLock(ctx, path)
	require.NoError(t, err)
	defer releaseConfigFileLock(path, lock)
	return f.store.RecoverCodexOAuthJournal(ctx, ScopeGlobal, f.used.ID)
}

func TestTask1292Engine_RefreshVariantsAndPolicy(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		for _, variant := range []string{"rotated", "identical", "empty"} {
			name := variant + "/drop-account"
			if preserve {
				name = variant + "/preserve-account"
			}
			t.Run(name, func(t *testing.T) {
				f := task1292Fixture(t, task1292Provider)
				task1292PauseWorkers(t)
				returned := *f.next
				switch variant {
				case "identical":
					returned.RefreshToken = f.used.OAuthToken.RefreshToken
				case "empty":
					returned.RefreshToken = ""
				}
				client := &http.Client{}
				calls := 0
				task1292Descriptor(t, preserve, func(_ context.Context, got *http.Client, refresh string) (*oauth.Token, error) {
					calls++
					require.Same(t, client, got)
					require.Equal(t, f.used.OAuthToken.RefreshToken, refresh)
					return &returned, nil
				})
				require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, client))
				require.Equal(t, 1, calls)
				pc, ok := f.store.Config().Providers.Get(f.used.ID)
				require.True(t, ok)
				require.Empty(t, pc.APIKey)
				want := returned
				if variant == "empty" {
					want.RefreshToken = f.used.OAuthToken.RefreshToken
					require.Empty(t, returned.RefreshToken, "normalization must not mutate exchange output")
				}
				if preserve {
					want.AccountID = f.used.OAuthToken.AccountID
				}
				require.Equal(t, &want, pc.OAuthToken)
				record, err := readOAuthJournal(oauthJournalPath(f.path, f.used.ID))
				require.NoError(t, err)
				require.NotNil(t, record)
				if variant == "rotated" {
					require.Equal(t, oauthRefreshTokenSHA256(f.used.OAuthToken), record.PredecessorRefreshTokenSHA256)
				} else {
					require.Empty(t, record.PredecessorRefreshTokenSHA256)
					require.Empty(t, record.BurnedRefreshTokenSHA256)
				}
				f.store.PublishMuForTest().Lock()
				pending, _ := f.store.OAuthCredStore().PendingRotationForTest(t, f.used.ID)
				candidate := f.store.LoadSnapshot().Clone()
				cfgCopy := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{f.used.ID: f.used})}
				*candidate.SnapConfigForTest() = cfgCopy
				f.store.OverlayPendingOAuthLocked(candidate)
				f.store.PublishMuForTest().Unlock()
				require.Equal(t, &want, pending.Record.Token)
				adopted, _ := (*candidate.SnapConfig()).Providers.Get(f.used.ID)
				require.Equal(t, &want, adopted.OAuthToken, "memory-first overlay retains replacement before config persistence")
				require.Empty(t, adopted.APIKey)
				disk, err := readOAuthJournalDiskToken(f.path, f.used.ID)
				require.NoError(t, err)
				require.Equal(t, f.used.OAuthToken, disk)
			})
		}
	}
}

func TestTask1292Engine_ConcurrentSingleExchangeAndStaleCaller(t *testing.T) {
	f := task1292Fixture(t, task1292Provider)
	task1292PauseWorkers(t)
	entered, release, waiter := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	task1292Descriptor(t, false, func(context.Context, *http.Client, string) (*oauth.Token, error) {
		calls++
		close(entered)
		select {
		case <-release:
			return f.next, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	restore := oauthcred.SetCodexRefreshWaiterHookForTest(func() {
		select {
		case waiter <- struct{}{}:
		default:
		}
	})
	t.Cleanup(restore)
	joinOAuthJournalTestStoresBeforeRestore(t)
	first := task1292Start(t, f.store, f.used)
	awaitCodexSignal(t, entered)
	second := task1292Start(t, f.store, f.used)
	awaitCodexSignal(t, waiter)
	close(release)
	require.NoError(t, awaitCodexResult(t, first))
	require.NoError(t, awaitCodexResult(t, second))
	require.Equal(t, 1, calls)
	require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
	require.Equal(t, 1, calls, "stale caller must not exchange again")
}

func TestTask1292Engine_StartupRecoveryAfterJournalHandoff(t *testing.T) {
	for _, refresh := range []string{"rotated", "identical", "empty"} {
		t.Run(refresh, func(t *testing.T) {
			f := task1292Fixture(t, task1292Provider)
			task1292PauseWorkers(t)
			b := *f.next
			switch refresh {
			case "identical":
				b.RefreshToken = f.used.OAuthToken.RefreshToken
			case "empty":
				b.RefreshToken = ""
			}
			calls := 0
			task1292Descriptor(t, true, func(context.Context, *http.Client, string) (*oauth.Token, error) {
				calls++
				return &b, nil
			})
			require.NoError(t, f.store.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
			pc, _ := f.store.Config().Providers.Get(f.used.ID)
			f.store.StopOAuthJournalWorkers()
			restarted := task1292Store(t, f.path, f.used)
			restarted.RecoverOAuthJournalsAtLoad()
			got, _ := restarted.Config().Providers.Get(f.used.ID)
			require.Equal(t, pc.OAuthToken, got.OAuthToken)
			require.Empty(t, got.APIKey)
			require.NoError(t, restarted.RefreshOAuthTokenWithClient(context.Background(), ScopeGlobal, f.used, nil))
			require.Equal(t, 1, calls, "startup-recovered token skips exchange")
			restarted.StopOAuthJournalWorkers()
			_, err := restarted.OAuthJournalWorkerAttempt(context.Background(), ScopeGlobal, f.used.ID, 0)
			require.NoError(t, err)
			_, record, err := task1292Recover(t, codexRefreshFixture{store: restarted, path: f.path, used: f.used})
			require.NoError(t, err)
			require.Nil(t, record)
			disk, err := readOAuthJournalDiskToken(f.path, f.used.ID)
			require.NoError(t, err)
			require.Equal(t, pc.OAuthToken, disk)
			data, err := os.ReadFile(f.path)
			require.NoError(t, err)
			var document map[string]map[string]map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &document))
			_, hasAPIKey := document["providers"][f.used.ID]["api_key"]
			require.False(t, hasAPIKey)
		})
	}
}
