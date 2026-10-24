// OAuth credential flows against a rush.ktav config slot: the store API
// must round-trip Ktav documents losslessly, refuse stale credential
// writes, and keep journals and refresh sidecars anchored on their
// documented spellings. Serial discipline mirrors the refresh-race
// tests: global seams are saved and restored via t.Cleanup, and journal
// workers are joined before the TempDir goes away (open handles break
// Windows cleanup).
package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/PHPCraftdream/rush/internal/oauth/codex"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// writeKtavFixture converts doc to Ktav with the production converter and
// writes it to path, returning the raw bytes on disk.
func writeKtavFixture(t *testing.T, path string, doc map[string]any) []byte {
	t.Helper()
	jsonBytes, err := json.Marshal(doc)
	require.NoError(t, err)
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(jsonBytes)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, ktavBytes, 0o600))
	return ktavBytes
}

// decodeKtavFixtureRaw reads the raw on-disk bytes of a rush.ktav file.
func decodeKtavFixtureRaw(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return raw
}

// decodeKtavFixtureJSON reads a rush.ktav file and returns the JSON-shaped
// bytes the production decode layer would produce.
func decodeKtavFixtureJSON(t *testing.T, path string) []byte {
	t.Helper()
	tree, err := ktavcodec.DecodeKtav(decodeKtavFixtureRaw(t, path))
	require.NoError(t, err)
	jsonBytes, err := ktavcodec.ToJSON(tree)
	require.NoError(t, err)
	return jsonBytes
}

// The credential set/delete API must persist into the active rush.ktav via
// the settings-document layer while every unknown field survives verbatim,
// and must never materialize a legacy rush.json sibling.
func TestOAuthKtavCredentialSetAndDeleteOnKtavConfig(t *testing.T) {
	ktavIsolateGlobals(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	configPath := filepath.Join(dir, "rush.ktav")
	writeKtavFixture(t, configPath, map[string]any{
		"providers": map[string]any{
			"custom-x": map[string]any{"id": "custom-x"},
			"custom-t": map[string]any{"id": "custom-t"},
		},
		"mcpServers": map[string]any{"srv": map[string]any{"url": "u"}},
		"custom_top": []string{"a"},
	})

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("custom-x", ProviderConfig{ID: "custom-x"})
	providers.Set("custom-t", ProviderConfig{ID: "custom-t"})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{Providers: providers},
		globalDataPath: configPath,
	})
	trackOAuthJournalTestStore(t, store)

	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "custom-x", "key-1"))
	jsonBytes := decodeKtavFixtureJSON(t, configPath)
	require.Equal(t, "key-1", gjson.Get(string(jsonBytes), "providers.custom-x.api_key").String())
	want := `{"custom_top":["a"],"mcpServers":{"srv":{"url":"u"}},` +
		`"providers":{"custom-t":{"id":"custom-t"},` +
		`"custom-x":{"api_key":"key-1","id":"custom-x"}}}`
	wantTree, err := ktavcodec.ParseJSONLossless([]byte(want))
	require.NoError(t, err)
	gotTree, err := ktavcodec.DecodeKtav(decodeKtavFixtureRaw(t, configPath))
	require.NoError(t, err)
	require.NoError(t, ktavcodec.Equivalent(wantTree, gotTree), "known fields plus unknown extras must survive the write")

	token := &oauth.Token{
		AccessToken:  "tok-access",
		RefreshToken: "tok-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		AccountID:    "tok-account",
	}
	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "custom-t", token))
	jsonBytes = decodeKtavFixtureJSON(t, configPath)
	require.Equal(t, "tok-access", gjson.Get(string(jsonBytes), "providers.custom-t.api_key").String())
	require.Equal(t, "tok-access", gjson.Get(string(jsonBytes), "providers.custom-t.oauth.access_token").String())
	require.Equal(t, "tok-refresh", gjson.Get(string(jsonBytes), "providers.custom-t.oauth.refresh_token").String())
	require.Equal(t, "u", gjson.Get(string(jsonBytes), "mcpServers.srv.url").String())
	require.Equal(t, "a", gjson.Get(string(jsonBytes), "custom_top.0").String())

	require.NoError(t, store.RemoveProviderAPIKey(ScopeGlobal, "custom-x"))
	jsonBytes = decodeKtavFixtureJSON(t, configPath)
	require.False(t, gjson.Get(string(jsonBytes), "providers.custom-x.api_key").Exists())
	require.True(t, gjson.Get(string(jsonBytes), "providers.custom-x.id").Exists())
	require.Equal(t, "u", gjson.Get(string(jsonBytes), "mcpServers.srv.url").String())
	_, statErr := os.Stat(filepath.Join(dir, "rush.json"))
	require.True(t, os.IsNotExist(statErr), "a rush.ktav slot must never grow a legacy rush.json sibling")
}

// A CAS write whose expected token no longer matches the Ktav document must
// be refused cleanly (committed=false, err=nil) and leave the raw bytes
// byte-for-byte unchanged.
func TestOAuthKtavStaleCredentialCASRefusalOnKtavConfig(t *testing.T) {
	ktavIsolateGlobals(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	configPath := filepath.Join(dir, "rush.ktav")
	tokenA := &oauth.Token{
		AccessToken:  "stale-old-access",
		RefreshToken: "stale-old-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		AccountID:    "stale-account",
	}
	writeKtavFixture(t, configPath, map[string]any{
		"providers": map[string]any{
			"p1": map[string]any{"oauth": tokenA},
		},
	})
	rawBefore, err := os.ReadFile(configPath)
	require.NoError(t, err)

	tokenB := *tokenA
	tokenB.AccessToken = "stale-other-access"
	tokenB.RefreshToken = "stale-other-refresh"
	writeKtavFixture(t, configPath, map[string]any{
		"providers": map[string]any{
			"p1": map[string]any{"oauth": &tokenB},
		},
	})

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("p1", ProviderConfig{ID: "p1", OAuthToken: tokenA})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{Providers: providers},
		globalDataPath: configPath,
	})
	trackOAuthJournalTestStore(t, store)
	tokenC := &oauth.Token{
		AccessToken:  "stale-new-access",
		RefreshToken: "stale-new-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(2 * time.Hour).Unix(),
		AccountID:    "stale-account",
	}
	committed, err := store.setOAuthTokenIfCurrent(ScopeGlobal, "p1", tokenA, tokenC)
	require.False(t, committed)
	require.NoError(t, err, "a lost CAS race is a clean refusal, not an error")
	rawAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, rawBefore, rawAfter, "a refused CAS write must not touch the raw bytes")
}

// Two stores rotating one single-use refresh token through ONE rush.ktav
// file: exactly one exchange wins, both stores converge on the new token,
// the file stays valid Ktav, and the refresh sidecar anchors on the legacy
// rush.json spelling. Ported from runOAuthRefreshSingleUseRace.
func TestOAuthKtavTwoStoreRefreshRotationOnSingleKtavFile(t *testing.T) {
	ktavIsolateGlobals(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	configPath := filepath.Join(dir, "rush.ktav")
	oldToken := &oauth.Token{
		AccessToken:  "single-use-old-access",
		RefreshToken: "single-use-old-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		AccountID:    "test-account",
	}
	writeKtavFixture(t, configPath, map[string]any{
		"providers": map[string]any{
			"openai-codex": map[string]any{"oauth": oldToken},
		},
	})

	makeStore := func() *ConfigStore {
		providers := csync.NewMap[string, ProviderConfig]()
		providers.Set("openai-codex", ProviderConfig{ID: "openai-codex", OAuthToken: oldToken})
		return newTestConfigStore(testStoreOpts{
			config:         &Config{Providers: providers},
			globalDataPath: configPath,
		})
	}
	stores := []*ConfigStore{makeStore(), makeStore()}

	firstArrived := make(chan struct{})
	reuseObserved := make(chan struct{})
	releaseSuccess := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSuccess) }) }
	var reuseOnce sync.Once
	var exchanges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if parseErr := r.ParseForm(); parseErr != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != oldToken.RefreshToken {
			t.Errorf("unexpected OAuth refresh form")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if exchanges.Add(1) != 1 {
			reuseOnce.Do(func() { close(reuseObserved) })
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		close(firstArrived)
		select {
		case <-releaseSuccess:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"single-use-new-access","refresh_token":"single-use-new-refresh","expires_in":3600}`))
	}))
	// LIFO cleanup releases a blocked handler before closing its server.
	t.Cleanup(server.Close)
	t.Cleanup(release)
	endpoint, err := url.Parse(server.URL)
	require.NoError(t, err)
	client := &http.Client{Transport: oauthRefreshLoopbackTransport{
		endpoint: endpoint,
		base:     server.Client().Transport,
	}}

	// Real exchange decoding; global seams require serial tests.
	originalRefresh := codexRefreshTokenFn
	codexRefreshTokenFn = codex.Refresh
	t.Cleanup(func() { codexRefreshTokenFn = originalRefresh })
	serialized := make(chan struct{}, 2)
	oldWaiterHook := codexRefreshWaiterHook
	codexRefreshWaiterHook = func() {
		select {
		case serialized <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { codexRefreshWaiterHook = oldWaiterHook })
	var lockAttempts atomic.Int32
	hookCodexRefreshLock(t, func(ctx context.Context, path string) (*filelock.FileLock, error) {
		if strings.Contains(path, ".oauth-refresh-") && lockAttempts.Add(1) > 1 {
			select {
			case serialized <- struct{}{}:
			default:
			}
		}
		return filelock.AcquireFileLockContext(ctx, path)
	})
	for _, store := range stores {
		trackOAuthJournalTestStore(t, store)
	}
	idle := hookOAuthJournalIdle(t, stores[0])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	results := make(chan error, 2)
	var workers sync.WaitGroup
	t.Cleanup(workers.Wait)
	// Cancel and unblock workers before waiting or restoring the global seam.
	t.Cleanup(func() {
		cancel()
		release()
	})
	entered := make(chan struct{}, 1)
	launch := func(store *ConfigStore, signal chan<- struct{}) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if signal != nil {
				signal <- struct{}{}
			}
			results <- store.RefreshOAuthTokenWithClient(ctx, ScopeGlobal, ProviderConfig{
				ID:         "openai-codex",
				OAuthToken: oldToken,
			}, client)
		}()
	}
	launch(stores[0], nil)
	select {
	case <-firstArrived:
	case <-ctx.Done():
		t.Fatal("first refresh never reached the loopback endpoint")
	}
	launch(stores[1], entered)
	<-entered
	select {
	case <-reuseObserved:
	case <-serialized:
	case <-ctx.Done():
		t.Fatal("refresh race exceeded its safety budget")
	}
	release()
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			assertOAuthRefreshRaceNoError(t, err)
		case <-ctx.Done():
			t.Fatal("refresh callers did not finish")
		}
	}
	workers.Wait()
	waitOAuthJournalIdle(t, idle)
	require.Equal(t, int32(1), exchanges.Load(), "a single-use token must be exchanged exactly once")
	for _, store := range stores {
		current, ok := store.loadSnapshot().config.Providers.Get("openai-codex")
		require.True(t, ok)
		require.NotNil(t, current.OAuthToken)
		require.Equal(t, "single-use-new-access", current.OAuthToken.AccessToken)
		require.Equal(t, "single-use-new-refresh", current.OAuthToken.RefreshToken)
	}
	diskToken, err := stores[0].loadTokenFromDisk(ScopeGlobal, "openai-codex")
	require.NoError(t, err)
	require.NotNil(t, diskToken)
	require.Equal(t, "single-use-new-access", diskToken.AccessToken)
	require.Equal(t, "single-use-new-refresh", diskToken.RefreshToken)

	jsonBytes := decodeKtavFixtureJSON(t, configPath)
	require.Equal(t, "single-use-new-refresh",
		gjson.Get(string(jsonBytes), "providers.openai-codex.oauth.refresh_token").String())
	_, statErr := os.Stat(filepath.Join(dir, "rush.json"))
	require.True(t, os.IsNotExist(statErr), "the Ktav slot must not grow a legacy sibling during rotation")
	expectedLock := oauthRefreshLockPath(configPath, "openai-codex")
	_, statErr = os.Stat(expectedLock)
	require.NoError(t, statErr, "the refresh sidecar must anchor on the legacy rush.json spelling")
	staleLocks, globErr := filepath.Glob(filepath.Join(dir, "rush.ktav.oauth-refresh-*.lock"))
	require.NoError(t, globErr)
	require.Empty(t, staleLocks, "no refresh sidecar may use the Ktav spelling")
}

// A corrupt rush.ktav must fail the CAS write with a real error (never the
// clean CAS refusal), leave the bytes untouched, and fail token reads
// instead of reporting a silently empty document.
func TestOAuthKtavCorruptConfigAbortsCASAndDiskReads(t *testing.T) {
	ktavIsolateGlobals(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	configPath := filepath.Join(dir, "rush.ktav")
	corrupt := []byte("not ktav at all")
	require.NoError(t, os.WriteFile(configPath, corrupt, 0o600))

	providers := csync.NewMap[string, ProviderConfig]()
	tokenA := &oauth.Token{AccessToken: "corrupt-old-access", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	providers.Set("p1", ProviderConfig{ID: "p1", OAuthToken: tokenA})
	store := newTestConfigStore(testStoreOpts{
		config:         &Config{Providers: providers},
		globalDataPath: configPath,
	})
	trackOAuthJournalTestStore(t, store)
	tokenC := &oauth.Token{AccessToken: "corrupt-new-access", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}

	committed, err := store.setOAuthTokenIfCurrent(ScopeGlobal, "p1", tokenA, tokenC)
	require.Error(t, err, "an undecodable document must abort the write")
	require.False(t, committed)
	require.False(t, errors.Is(err, errOAuthCredentialCAS), "a decode failure is not a CAS refusal")
	rawAfter, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, corrupt, rawAfter)

	_, err = store.loadTokenFromDisk(ScopeGlobal, "p1")
	require.Error(t, err, "a corrupt document must never read as an empty config")
	_, err = readOAuthJournalDiskToken(configPath, "p1")
	require.ErrorIs(t, err, errOAuthJournalDisk)
}

// Journal spellings follow the config slot: an existing Ktav journal wins
// over the legacy JSON one, both decode through the production reader, and
// an invalid preferred journal is refused and quarantined byte-for-byte.
func TestOAuthKtavJournalSpellingPrecedenceAndQuarantine(t *testing.T) {
	ktavIsolateGlobals(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	configPath := filepath.Join(dir, "rush.json")
	provider := "openai-codex"
	record := &oauthJournalRecord{Token: &oauth.Token{
		AccessToken:  "journal-access",
		RefreshToken: "journal-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}}

	// Legacy journal only: it stays the target and reads back.
	legacyPath := oauthJournalPath(configPath, provider)
	require.NoError(t, writeOAuthJournalOnce(legacyPath, record))
	require.Equal(t, normalizeReloadPath(legacyPath),
		normalizeReloadPath(selectOAuthJournalPath(configPath, provider, false)))
	legacyRead, err := readOAuthJournal(legacyPath)
	require.NoError(t, err)
	require.Equal(t, record, legacyRead)

	// Ktav journal now exists too: it must win selection and decode equal.
	ktavPath := oauthJournalKtavPath(configPath, provider)
	require.NoError(t, writeOAuthJournalOnce(ktavPath, record))
	require.Equal(t, normalizeReloadPath(ktavPath),
		normalizeReloadPath(selectOAuthJournalPath(configPath, provider, false)))
	ktavRead, err := readOAuthJournal(ktavPath)
	require.NoError(t, err)
	require.Equal(t, record, ktavRead)

	// An invalid preferred journal is refused, never re-parsed as JSON, and
	// its raw bytes survive quarantine under path+".corrupt-<n>".
	garbage := []byte("garbage journal bytes")
	require.NoError(t, os.WriteFile(ktavPath, garbage, 0o600))
	_, err = readOAuthJournal(ktavPath)
	require.ErrorIs(t, err, errOAuthJournalInvalid)
	quarantineOAuthJournal(ktavPath)
	quarantined, err := filepath.Glob(ktavPath + ".corrupt-*")
	require.NoError(t, err)
	require.NotEmpty(t, quarantined)
	preserved, err := os.ReadFile(quarantined[0])
	require.NoError(t, err)
	require.Equal(t, garbage, preserved)
}

// With the auto-migrate gate on, a journal WRITE for a slot with neither
// spelling creates the Ktav document (raw bytes are not JSON); with the
// default gate off it stays the plain-JSON legacy name. Serial: the package
// gate var is saved and restored via t.Cleanup.
func TestOAuthKtavGatedJournalWriteSpelling(t *testing.T) {
	ktavIsolateGlobals(t)
	provider := "openai-codex"
	record := &oauthJournalRecord{Token: &oauth.Token{
		AccessToken:  "gated-access",
		RefreshToken: "gated-refresh",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}}
	savedGate := ktavAutoMigrate
	t.Cleanup(func() { ktavAutoMigrate = savedGate })

	// Gate on: the write creates the Ktav journal spelling.
	ktavAutoMigrate = true
	dirOn, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	configOn := filepath.Join(dirOn, "rush.json")
	ktavPath := oauthJournalKtavPath(configOn, provider)
	require.Equal(t, normalizeReloadPath(ktavPath),
		normalizeReloadPath(selectOAuthJournalPath(configOn, provider, true)))
	require.NoError(t, writeOAuthJournalOnce(ktavPath, record))
	raw, err := os.ReadFile(ktavPath)
	require.NoError(t, err)
	var unused map[string]any
	require.Error(t, json.Unmarshal(raw, &unused), "Ktav journal bytes must not be plain JSON")
	jsonBytes, err := decodeConfigDocument(ktavPath, raw)
	require.NoError(t, err)
	var decoded oauthJournalRecord
	require.NoError(t, json.Unmarshal(jsonBytes, &decoded))
	require.Equal(t, record, &decoded)

	// Default gate off: the write targets the legacy plain-JSON name.
	ktavAutoMigrate = false
	dirOff, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	configOff := filepath.Join(dirOff, "rush.json")
	legacyPath := oauthJournalPath(configOff, provider)
	require.Equal(t, normalizeReloadPath(legacyPath),
		normalizeReloadPath(selectOAuthJournalPath(configOff, provider, true)))
	require.NoError(t, writeOAuthJournalOnce(legacyPath, record))
	legacyRaw, err := os.ReadFile(legacyPath)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(legacyRaw, &unused), "the legacy journal must stay plain JSON")
	_, statErr := os.Stat(oauthJournalKtavPath(configOff, provider))
	require.True(t, os.IsNotExist(statErr), "the gated-off write must not create a Ktav journal")
}

// Pure-function check: the refresh sidecar for a rush.ktav config anchors on
// the legacy rush.json spelling, with the documented hash suffix.
func TestOAuthKtavRefreshLockPathAnchorsOnLegacySpelling(t *testing.T) {
	dir := t.TempDir()
	ktavPath := filepath.Join(dir, "rush.ktav")
	legacyPath := filepath.Join(dir, "rush.json")
	fromKtav := oauthRefreshLockPath(ktavPath, "openai-codex")
	fromLegacy := oauthRefreshLockPath(legacyPath, "openai-codex")
	require.Equal(t, fromLegacy, fromKtav)
	digest := sha256.Sum256([]byte("openai-codex"))
	suffix := ".oauth-refresh-" + hex.EncodeToString(digest[:8]) + ".lock"
	require.Contains(t, fromKtav, "rush.json.oauth-refresh-")
	require.True(t, strings.HasSuffix(fromKtav, suffix),
		"lock name must end with %q, got %q", suffix, fromKtav)
}
