// REVERT-CHECKS (orchestrator runs the mutants); one line per test.
// TestModelsIntegrity_SignsModelsWrite -> deleting the touchesModelsKey(keys) block in SetConfigFields (store_write.go:376).
// TestModelsIntegrity_NonModelsWritesLeaveSignature -> making applyModelsIntegrity unconditional in SetConfigFields (store_write.go:376): the signature is recomputed over the TAMPERED models during the housekeeping writes, so the cache_rev byte-identity assertions fail.
// TestModelsIntegrity_DirectEditMismatch -> weakening the comparison in verifyModelsIntegrityAtLoad (settings_models_integrity.go).
// TestModelsIntegrity_BogusSignatureMismatch -> replacing the mismatch comparison at the top of verifyModelsIntegrityAtLoad with unconditional acceptance (settings_models_integrity.go).
// TestModelsIntegrity_PresentInvalidSignatureMismatch -> reverting the gjson Result check back to the `.String() == ""` collapse in verifyModelsIntegrityAtLoad (settings_models_integrity.go:143-152).
// TestModelsIntegrity_WhitespaceReformatStillOK -> canonicalizing with raw bytes instead of UseNumber decode+marshal (settings_models_integrity.go:58).
// TestModelsIntegrity_MissingKeyAcceptedWithoutLock -> dropping the `!rev.Exists()` acceptance branch (settings_models_integrity.go:145).
// TestModelsIntegrity_MissingKeyWithLockMismatch -> removing the anySettingsLockExists call (settings_models_integrity.go:148) or its fail-closed read-error handling.
// TestModelsIntegrity_LockSettingsSigns -> removing applyModelsIntegrity from LockSettings (settings_lock.go:306).
// TestModelsIntegrity_RemoveModelsRemovesKey -> removing the models branch in removeConfigFieldAt (store_write.go:537).
// TestModelsIntegrity_ReloadDoesNotReVerify -> calling verifyModelsIntegrityAtLoad from reloadFromDiskLocked (store_reload.go).
// TestModelsIntegrity_ErrorTextAndOutputsRedacted -> rewording ErrModelsEditedDirectly or un-redacting the Warn (settings_models_integrity.go:23/166).
// TestModelsIntegrity_WorkspaceVerifiedSeparately -> dropping workspacePath from the file loop in verifyModelsIntegrityAtLoad (settings_models_integrity.go:123).
// TestModelsIntegrity_MismatchNeverBlocksLoadOrWrites -> returning ErrModelsEditedDirectly from Load (load.go:278/294).
// TestModelsIntegrity_CanonicalForms -> breaking canonicalModelsJSON (absent/empty detection, UseNumber).
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// integrityIsolate redirects every global config path to fresh temp dirs and
// returns a temp workspace dir; call it before any store is constructed.
func integrityIsolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	isolateAllGlobalConfigPaths(t)
	resetProviderState()
	t.Cleanup(resetProviderState)
	return dir
}

// integrityModelsFixture returns an offline, deterministic rush.json body.
// modelsJSON may be "" to omit the models object entirely.
func integrityModelsFixture(t *testing.T, dir string, modelsJSON string) string {
	t.Helper()
	doc := `{"options":{"disable_default_providers":true}`
	if modelsJSON != "" {
		doc += `,"models":` + modelsJSON
	}
	doc += `,"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}}`
	return doc
}

// writeIntegrityFixture seeds path with the offline fixture and returns the bytes.
func writeIntegrityFixture(t *testing.T, path string, modelsJSON string) []byte {
	t.Helper()
	data := []byte(integrityModelsFixture(t, path, modelsJSON))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return data
}

// signFileOnDisk applies the real applyModelsIntegrity helper to the file's
// current bytes (adds/refreshes or removes cache_rev) and writes it back.
func signFileOnDisk(t *testing.T, path string) {
	t.Helper()
	signed, err := applyModelsIntegrity(string(fileBytes(t, path)))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(signed), 0o600))
}

// editModelsOnDisk edits a model id directly in the file, like a hand edit,
// and leaves the original cache_rev byte-for-byte untouched (stale signature).
func editModelsOnDisk(t *testing.T, path, modelID string) {
	t.Helper()
	raw := string(fileBytes(t, path))
	edited, err := sjson.Set(raw, "models.smart.model", modelID)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte(edited), 0o600))
}

func expectedRevFor(t *testing.T, data []byte) string {
	t.Helper()
	canonical, present, err := canonicalModelsJSON(gjson.GetBytes(data, "models").Raw)
	require.NoError(t, err)
	require.True(t, present)
	return modelsIntegritySignature(canonical)
}

// integrityLoadHarness isolates global paths and returns the workspace
// config path (= dir/rush.json, the WORKSPACE scope file) and the GLOBAL
// scope file path under RUSH_GLOBAL_DATA.
func integrityLoadHarness(t *testing.T) (dir, configPath, globalPath string) {
	t.Helper()
	dir = integrityIsolate(t)
	configPath = filepath.Join(dir, "rush.json")
	globalPath = GlobalConfigData()
	return dir, configPath, globalPath
}

// TestModelsIntegrity_SignsModelsWrite pins that a models write persists a
// cache_rev exactly matching the real signature over the canonical models.
func TestModelsIntegrity_SignsModelsWrite(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir := integrityIsolate(t)
	p := filepath.Join(dir, "global.json")
	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: p})
	s.workingDir = dir
	require.NoError(t, s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "p", Model: "m"}))
	data := fileBytes(t, p)
	require.Equal(t, expectedRevFor(t, data), gjson.GetBytes(data, "cache_rev").String())
}

// TestModelsIntegrity_NonModelsWritesLeaveSignature pins that writes which do
// not touch models leave the existing cache_rev byte-identical, even when the
// models under that stale signature were edited directly on disk.
func TestModelsIntegrity_NonModelsWritesLeaveSignature(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir := integrityIsolate(t)
	globalPath := GlobalConfigData()
	require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))

	// Seed providers.hyper (expired OAuth) AND models in one initial file;
	// the models write below signs the whole document, providers included.
	// providers.openai keeps Load's provider validation satisfied while
	// default providers are disabled.
	seed := `{"options":{"disable_default_providers":true},` +
		`"models":{"smart":{"provider":"p","model":"m"}},` +
		`"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]},` +
		`"hyper":{"api_key":"old-access","base_url":"https://hyper.example/v1","models":[{"id":"h1","name":"H1"}],"oauth":{"access_token":"old-access","refresh_token":"refresh","expires_in":3600,"expires_at":1}}}}`
	require.NoError(t, os.WriteFile(globalPath, []byte(seed), 0o600))
	signedDoc, err := applyModelsIntegrity(string(fileBytes(t, globalPath)))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(globalPath, []byte(signedDoc), 0o600))

	SetProcessPassword("")
	t.Cleanup(func() { SetProcessPassword("") })
	s, err := Load(dir, filepath.Dir(globalPath), false)
	require.NoError(t, err)
	require.NoError(t, s.ModelsIntegrity())

	// Tamper ONLY the models content; cache_rev stays byte-for-byte the
	// signed value, so the file now carries a stale signature.
	editModelsOnDisk(t, globalPath, "tampered")
	stale := gjson.GetBytes(fileBytes(t, globalPath), "cache_rev").String()
	require.NotEmpty(t, stale)

	orig := hyperExchangeTokenFn
	hyperExchangeTokenFn = func(context.Context, *http.Client, string) (*oauth.Token, error) {
		return &oauth.Token{AccessToken: "fresh", RefreshToken: "fresh-r", ExpiresIn: 7200, ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	}
	t.Cleanup(func() { hyperExchangeTokenFn = orig })

	for name, write := range map[string]func() error{
		"recent_models": func() error {
			return s.SetConfigField(ScopeGlobal, "recent_models.smart", []SelectedModel{{Provider: "p", Model: "m"}})
		},
		// Targets providers.openai, NOT hyper: the OAuth CAS requires
		// providers.hyper.api_key == providers.hyper.oauth.access_token.
		"provider_api_key": func() error {
			return s.SetConfigField(ScopeGlobal, "providers.openai.api_key", "k")
		},
		"oauth_refresh": func() error {
			return s.RefreshOAuthToken(context.Background(), ScopeGlobal, "hyper")
		},
		"mcp": func() error {
			return s.PersistMCPConfig(ScopeGlobal, "srv", MCPConfig{Type: MCPHttp, URL: "http://x"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, write())
			require.Equal(t, stale, gjson.GetBytes(fileBytes(t, globalPath), "cache_rev").String())
		})
	}

	// No housekeeping write blessed the tampered models: a fresh Load must
	// still record the mismatch against the stale signature.
	fresh, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.ErrorIs(t, fresh.ModelsIntegrity(), ErrModelsEditedDirectly)
}

// TestModelsIntegrity_DirectEditMismatch pins the full real-Load flow: after
// rush signs a models write, a direct hand edit of the model id (keeping the
// ORIGINAL valid signature) makes the next Load record ErrModelsEditedDirectly.
func TestModelsIntegrity_DirectEditMismatch(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir, configPath, globalPath := integrityLoadHarness(t)
	writeIntegrityFixture(t, configPath, "") // workspace: providers only

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NoError(t, store.ModelsIntegrity())

	// Signed models write through the API.
	require.NoError(t, store.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "p", Model: "m"}))

	// Direct hand edit of the signed file; the signature is not refreshed.
	editModelsOnDisk(t, globalPath, "hacked")

	fresh, err := Load(dir, dir, false)
	require.NoError(t, err, "a mismatch must never fail Load")
	require.ErrorIs(t, fresh.ModelsIntegrity(), ErrModelsEditedDirectly)
}

// TestModelsIntegrity_BogusSignatureMismatch pins that an intact models object
// carrying a bogus (non-signature) cache_rev is a mismatch at Load.
func TestModelsIntegrity_BogusSignatureMismatch(t *testing.T) {
	dir, configPath, globalPath := integrityLoadHarness(t)
	writeIntegrityFixture(t, configPath, "")
	global := writeIntegrityFixture(t, globalPath, `{"smart":{"provider":"p","model":"m"}}`)
	bogus, err := sjson.SetBytes(global, "cache_rev", strings.Repeat("0", 64))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(globalPath, bogus, 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
}

// TestModelsIntegrity_WhitespaceReformatStillOK pins that reformatting
// (key reordering + whitespace) of a signed file is NOT a mismatch, because
// the signature is over canonicalized JSON.
func TestModelsIntegrity_WhitespaceReformatStillOK(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir, configPath, _ := integrityLoadHarness(t)
	writeIntegrityFixture(t, configPath, `{"smart":{"provider":"p","model":"m"}}`)
	signFileOnDisk(t, configPath)

	t.Run("compact re-marshal reorders keys", func(t *testing.T) {
		var decoded any
		decoder := json.NewDecoder(bytes.NewReader(fileBytes(t, configPath)))
		decoder.UseNumber()
		require.NoError(t, decoder.Decode(&decoded))
		compact, err := json.Marshal(decoded)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(configPath, compact, 0o600))

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.NoError(t, store.ModelsIntegrity())
	})

	t.Run("pretty indented whitespace", func(t *testing.T) {
		var pretty bytes.Buffer
		require.NoError(t, json.Indent(&pretty, fileBytes(t, configPath), "", "  "))
		require.NoError(t, os.WriteFile(configPath, pretty.Bytes(), 0o600))

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.NoError(t, store.ModelsIntegrity())
	})
}

// TestModelsIntegrity_MissingKeyAcceptedWithoutLock pins that a models object
// without cache_rev and with no settings lock anywhere is accepted at Load.
func TestModelsIntegrity_MissingKeyAcceptedWithoutLock(t *testing.T) {
	dir, configPath, _ := integrityLoadHarness(t)
	writeIntegrityFixture(t, configPath, `{"smart":{"provider":"p","model":"m"}}`)

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NoError(t, store.ModelsIntegrity())
}

// TestModelsIntegrity_MissingKeyWithLockMismatch pins that a missing cache_rev
// is only a mismatch when a settings lock exists (fails closed on read errors).
func TestModelsIntegrity_MissingKeyWithLockMismatch(t *testing.T) {
	t.Run("global lock", func(t *testing.T) {
		dir, configPath, globalPath := integrityLoadHarness(t)
		writeIntegrityFixture(t, configPath, "")
		global := writeIntegrityFixture(t, globalPath, `{"smart":{"provider":"p","model":"m"}}`)
		locked, err := sjson.SetBytes(global, "sync_rev", HashPassword("pw"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(globalPath, locked, 0o600))

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
	})

	t.Run("workspace lock", func(t *testing.T) {
		dir, configPath, _ := integrityLoadHarness(t)
		workspace := `{"sync_rev":"` + HashPassword("pw") + `","models":{"smart":{"provider":"p","model":"m"}},"options":{"disable_default_providers":true},"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}}`
		require.NoError(t, os.WriteFile(configPath, []byte(workspace), 0o600))

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
	})

	t.Run("read error fails closed", func(t *testing.T) {
		integrityIsolate(t)
		// Make GlobalConfigData() a DIRECTORY: every read on it errors, and
		// anySettingsLockExists must treat a read error as "lock exists".
		require.NoError(t, os.MkdirAll(GlobalConfigData(), 0o755))
		s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: filepath.Join(t.TempDir(), "global.json")})
		s.workingDir = t.TempDir()
		require.True(t, s.anySettingsLockExists())
	})
}

// TestModelsIntegrity_PresentInvalidSignatureMismatch pins that a PRESENT but
// invalid cache_rev (empty string, null, number, object) is a mismatch at a
// real Load, regardless of any settings lock.
func TestModelsIntegrity_PresentInvalidSignatureMismatch(t *testing.T) {
	providers := `"providers":{"openai":{"api_key":"test-key","base_url":"https://example.invalid/v1","models":[{"id":"gpt-4","name":"GPT-4"}]}}`
	models := `"models":{"smart":{"provider":"p","model":"m"}}`
	for name, rawRev := range map[string]string{
		"empty string": `""`,
		"null":         `null`,
		"number":       `123`,
		"object":       `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, configPath, globalPath := integrityLoadHarness(t)
			writeIntegrityFixture(t, configPath, "") // workspace: providers only
			doc := `{"cache_rev":` + rawRev + `,` + models + `,"options":{"disable_default_providers":true},` + providers + `}`
			require.NoError(t, os.MkdirAll(filepath.Dir(globalPath), 0o755))
			require.NoError(t, os.WriteFile(globalPath, []byte(doc), 0o600))

			store, err := Load(dir, dir, false)
			require.NoError(t, err)
			require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
		})
	}

	t.Run("absent without lock accepted", func(t *testing.T) {
		dir, configPath, globalPath := integrityLoadHarness(t)
		writeIntegrityFixture(t, configPath, "")
		writeIntegrityFixture(t, globalPath, `{"smart":{"provider":"p","model":"m"}}`)

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.NoError(t, store.ModelsIntegrity())
	})

	t.Run("absent with lock mismatch", func(t *testing.T) {
		dir, configPath, globalPath := integrityLoadHarness(t)
		writeIntegrityFixture(t, configPath, "")
		global := writeIntegrityFixture(t, globalPath, `{"smart":{"provider":"p","model":"m"}}`)
		locked, err := sjson.SetBytes(global, "sync_rev", HashPassword("pw"))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(globalPath, locked, 0o600))

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
	})
}

// TestModelsIntegrity_LockSettingsSigns pins that LockSettings (re)signs
// cache_rev over the file's CURRENT models content in the same write.
func TestModelsIntegrity_LockSettingsSigns(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir, configPath, globalPath := integrityLoadHarness(t)
	writeIntegrityFixture(t, configPath, "")
	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "p", Model: "m"}))

	t.Cleanup(func() { SetProcessPassword("") })
	SetProcessPassword("pw")
	require.NoError(t, store.LockSettings(ScopeGlobal, "pw"))
	data := fileBytes(t, globalPath)
	require.Equal(t, expectedRevFor(t, data), gjson.GetBytes(data, "cache_rev").String())

	// Edit models on disk (stale signature), then lock again: the lock write
	// must re-sign over the edited content, and a fresh Load must accept it.
	editModelsOnDisk(t, globalPath, "tampered")
	require.NoError(t, store.LockSettings(ScopeGlobal, "pw"))
	data = fileBytes(t, globalPath)
	require.Equal(t, expectedRevFor(t, data), gjson.GetBytes(data, "cache_rev").String())

	fresh, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NoError(t, fresh.ModelsIntegrity())
}

// TestModelsIntegrity_RemoveModelsRemovesKey pins that removing the models
// object also removes cache_rev (absent models => no signature).
func TestModelsIntegrity_RemoveModelsRemovesKey(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir := integrityIsolate(t)
	p := filepath.Join(dir, "global.json")
	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: p})
	s.workingDir = dir
	require.NoError(t, s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "p", Model: "m"}))
	require.True(t, gjson.GetBytes(fileBytes(t, p), "cache_rev").Exists())

	require.NoError(t, s.RemoveConfigField(ScopeGlobal, "models"))
	require.False(t, gjson.GetBytes(fileBytes(t, p), "cache_rev").Exists())

	// The file has no providers; copy a provider in so Load stays offline.
	withProvider, err := sjson.Set(string(fileBytes(t, p)), "providers.openai", map[string]any{
		"api_key": "test-key", "base_url": "https://example.invalid/v1",
		"models": []map[string]string{{"id": "gpt-4", "name": "GPT-4"}},
	})
	require.NoError(t, err)
	withOpts, err := sjson.Set(withProvider, "options.disable_default_providers", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "rush.json"), []byte(withOpts), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.NoError(t, store.ModelsIntegrity())
}

// TestModelsIntegrity_ReloadDoesNotReVerify pins that ReloadFromDisk does not
// re-run the startup check in either direction (set-once semantics).
func TestModelsIntegrity_ReloadDoesNotReVerify(t *testing.T) {
	t.Run("clean load stays nil after tamper", func(t *testing.T) {
		dir, configPath, _ := integrityLoadHarness(t)
		writeIntegrityFixture(t, configPath, `{"smart":{"provider":"p","model":"m"}}`)
		signFileOnDisk(t, configPath)
		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.NoError(t, store.ModelsIntegrity())

		editModelsOnDisk(t, configPath, "hacked")
		require.NoError(t, store.ReloadFromDisk(context.Background()))
		require.NoError(t, store.ModelsIntegrity())
	})

	t.Run("recorded mismatch survives repair reload", func(t *testing.T) {
		dir, configPath, globalPath := integrityLoadHarness(t)
		writeIntegrityFixture(t, configPath, "")
		global := writeIntegrityFixture(t, globalPath, `{"smart":{"provider":"p","model":"m"}}`)
		tampered, err := sjson.SetBytes(global, "cache_rev", strings.Repeat("0", 64))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(globalPath, tampered, 0o600))

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)

		// Repair via the API (SetConfigField auto-reloads) — the recorded
		// mismatch must still be returned.
		require.NoError(t, store.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "p", Model: "m"}))
		require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
	})
}

// TestModelsIntegrity_ErrorTextAndOutputsRedacted pins the exact error text
// and that neither the Warn log nor the audit event leaks cache_rev/hashes.
func TestModelsIntegrity_ErrorTextAndOutputsRedacted(t *testing.T) {
	require.EqualError(t, ErrModelsEditedDirectly,
		"models cannot be changed by editing the settings file directly; they can only be changed with the rush models commands (ask the user if that is refused)")

	// Audit event for a models write: keys name the target, never cache_rev.
	dir := integrityIsolate(t)
	p := filepath.Join(dir, "global.json")
	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: p})
	s.workingDir = dir
	var captured SettingsEvent
	SetSettingsAuditSink(func(event SettingsEvent) { captured = event })
	t.Cleanup(func() { SetSettingsAuditSink(nil) })
	require.NoError(t, s.SetConfigField(ScopeGlobal, "models.smart", SelectedModel{Provider: "p", Model: "m"}))
	require.Contains(t, captured.Keys, "models.smart")
	for _, key := range captured.Keys {
		require.NotContains(t, key, "cache_rev")
	}

	// Warn during a mismatching Load: message present, secrets absent.
	dir, configPath, _ := integrityLoadHarness(t)
	writeIntegrityFixture(t, configPath, `{"smart":{"provider":"p","model":"m"}}`)
	signFileOnDisk(t, configPath)
	expectedRev := gjson.GetBytes(fileBytes(t, configPath), "cache_rev").String()
	editModelsOnDisk(t, configPath, "hacked")

	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	done := make(chan struct{})
	var store *ConfigStore
	var loadErr error
	go func() {
		defer close(done)
		store, loadErr = Load(dir, dir, false)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Load hung past 20s")
	}
	require.NoError(t, loadErr)
	require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)

	text := strings.ToLower(output.String())
	require.Contains(t, text, "model settings were changed outside rush")
	require.NotContains(t, strings.ToLower(output.String()), "cache_rev")
	require.NotContains(t, strings.ToLower(output.String()), expectedRev)
	for _, secret := range []string{"lock", "password", "unlock"} {
		require.NotContains(t, text, secret)
	}
}

// TestModelsIntegrity_WorkspaceVerifiedSeparately pins that BOTH the global
// data file and the workspace file are verified, independently.
func TestModelsIntegrity_WorkspaceVerifiedSeparately(t *testing.T) {
	models := `{"smart":{"provider":"p","model":"m"}}`

	t.Run("global signed workspace tampered", func(t *testing.T) {
		dir, configPath, globalPath := integrityLoadHarness(t)
		writeIntegrityFixture(t, configPath, models)
		writeIntegrityFixture(t, globalPath, models)
		signFileOnDisk(t, globalPath)
		signFileOnDisk(t, configPath)
		editModelsOnDisk(t, configPath, "hacked")

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
	})

	t.Run("global tampered workspace signed", func(t *testing.T) {
		dir, configPath, globalPath := integrityLoadHarness(t)
		writeIntegrityFixture(t, configPath, models)
		writeIntegrityFixture(t, globalPath, models)
		signFileOnDisk(t, configPath)
		signFileOnDisk(t, globalPath)
		editModelsOnDisk(t, globalPath, "hacked")

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
	})

	t.Run("both signed", func(t *testing.T) {
		dir, configPath, globalPath := integrityLoadHarness(t)
		writeIntegrityFixture(t, configPath, models)
		writeIntegrityFixture(t, globalPath, models)
		signFileOnDisk(t, configPath)
		signFileOnDisk(t, globalPath)

		store, err := Load(dir, dir, false)
		require.NoError(t, err)
		require.NoError(t, store.ModelsIntegrity())
	})
}

// TestModelsIntegrity_MismatchNeverBlocksLoadOrWrites pins that a mismatch is
// recorded but never fails Load and never rewrites the file.
func TestModelsIntegrity_MismatchNeverBlocksLoadOrWrites(t *testing.T) {
	pinKtavAutoMigrateOffForLegacyTest(t)
	dir, configPath, _ := integrityLoadHarness(t)
	writeIntegrityFixture(t, configPath, `{"smart":{"provider":"p","model":"m"}}`)
	tampered, err := sjson.Set(string(fileBytes(t, configPath)), "cache_rev", strings.Repeat("a", 64))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, []byte(tampered), 0o600))
	before := fileBytes(t, configPath)

	store, err := Load(dir, dir, false)
	require.NoError(t, err, "a mismatch must never fail Load")
	require.NotNil(t, store)
	require.ErrorIs(t, store.ModelsIntegrity(), ErrModelsEditedDirectly)
	require.Equal(t, before, fileBytes(t, configPath), "Load must not write")
}

// TestModelsIntegrity_CanonicalForms pins canonicalModelsJSON semantics:
// absent/null/{} are not present; key order and whitespace are normalized;
// numeric literals stay distinct thanks to UseNumber.
func TestModelsIntegrity_CanonicalForms(t *testing.T) {
	integrityIsolate(t)
	for _, raw := range []string{"", "null"} {
		_, present, err := canonicalModelsJSON(raw)
		require.NoError(t, err)
		require.False(t, present, "raw %q", raw)
	}
	_, present, err := canonicalModelsJSON("{}")
	require.NoError(t, err)
	require.False(t, present)

	a, present, err := canonicalModelsJSON(`{"b":1,"a":2}`)
	require.NoError(t, err)
	require.True(t, present)
	b, present, err := canonicalModelsJSON("{ \n \"a\" : 2 , \"b\" : 1 }")
	require.NoError(t, err)
	require.True(t, present)
	require.Equal(t, a, b)

	n1, _, err := canonicalModelsJSON(`{"price":1.50}`)
	require.NoError(t, err)
	n2, _, err := canonicalModelsJSON(`{"price":1.5}`)
	require.NoError(t, err)
	require.NotEqual(t, n1, n2, "numeric literals must stay distinct (UseNumber)")
}

// ---------------------------------------------------------------------------
// Ktav-source cases (ADDITIVE — every JSON-source assertion above is
// untouched). A .ktav document is verified via the SAME modelsIntegrityOf on
// its DECODED JSON bytes (decodeSettingsDocument -> ktavcodec), so the
// signature must be computed over the Ktav-normalized models lexemes.
// ---------------------------------------------------------------------------

// ktavIntegrityWrite writes jsonDoc to path as a rush.ktav document and
// returns the decoded JSON bytes of what is on disk.
func ktavIntegrityWrite(t *testing.T, path, jsonDoc string) []byte {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, testKtavBytes(t, jsonDoc), 0o600))
	return mustDecodeTestKtav(t, fileBytes(t, path))
}

// ktavIntegrityStore returns a store whose global file is the .ktav document
// at path, ready for verifyModelsIntegrityAtLoad.
func ktavIntegrityStore(t *testing.T, path string) *ConfigStore {
	t.Helper()
	s := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: path})
	s.workingDir = t.TempDir()
	return s
}

// TestModelsIntegrityKtav_ValidSignaturePasses pins that a valid signature
// computed over the canonical models verifies against a .ktav document,
// through both modelsIntegrityOf (on the decoded bytes) and the real
// verifyModelsIntegrityAtLoad path.
func TestModelsIntegrityKtav_ValidSignaturePasses(t *testing.T) {
	integrityIsolate(t)
	ktavPath := filepath.Join(t.TempDir(), "rush.ktav")
	doc := integrityModelsFixture(t, ktavPath, `{"smart":{"provider":"p","model":"m"}}`)
	signed, err := applyModelsIntegrity(doc)
	require.NoError(t, err)
	decoded := ktavIntegrityWrite(t, ktavPath, signed)

	require.NoError(t, modelsIntegrityOf(decoded, false))
	require.NoError(t, modelsIntegrityOf(decoded, true), "a valid signature passes locked or not")

	s := ktavIntegrityStore(t, ktavPath)
	s.verifyModelsIntegrityAtLoad()
	require.NoError(t, s.ModelsIntegrity())
}

// TestModelsIntegrityKtav_MissingSignatureFollowsJSONRule pins that a .ktav
// document whose models carry no cache_rev behaves exactly like the legacy
// JSON case: accepted when no settings lock exists, a mismatch when one does.
func TestModelsIntegrityKtav_MissingSignatureFollowsJSONRule(t *testing.T) {
	integrityIsolate(t)
	models := `{"smart":{"provider":"p","model":"m"}}`

	t.Run("accepted without lock", func(t *testing.T) {
		ktavPath := filepath.Join(t.TempDir(), "rush.ktav")
		decoded := ktavIntegrityWrite(t, ktavPath, integrityModelsFixture(t, ktavPath, models))
		require.NoError(t, modelsIntegrityOf(decoded, false))
		s := ktavIntegrityStore(t, ktavPath)
		s.verifyModelsIntegrityAtLoad()
		require.NoError(t, s.ModelsIntegrity())
	})

	t.Run("mismatch with lock", func(t *testing.T) {
		ktavPath := filepath.Join(t.TempDir(), "rush.ktav")
		doc := integrityModelsFixture(t, ktavPath, models)
		locked, err := sjson.Set(doc, "sync_rev", HashPassword("pw"))
		require.NoError(t, err)
		decoded := ktavIntegrityWrite(t, ktavPath, locked)
		require.ErrorIs(t, modelsIntegrityOf(decoded, true), ErrModelsEditedDirectly)
		s := ktavIntegrityStore(t, ktavPath)
		s.verifyModelsIntegrityAtLoad()
		require.ErrorIs(t, s.ModelsIntegrity(), ErrModelsEditedDirectly)
	})
}

// TestModelsIntegrityKtav_InvalidSignatureFails pins that an invalid (bogus
// string or non-string) cache_rev on a .ktav document is a mismatch, and that
// an UNDECODABLE .ktav fails closed at the load-time check.
func TestModelsIntegrityKtav_InvalidSignatureFails(t *testing.T) {
	integrityIsolate(t)

	t.Run("bogus string signature", func(t *testing.T) {
		ktavPath := filepath.Join(t.TempDir(), "rush.ktav")
		doc := integrityModelsFixture(t, ktavPath, `{"smart":{"provider":"p","model":"m"}}`)
		bogus, err := sjson.Set(doc, "cache_rev", strings.Repeat("0", 64))
		require.NoError(t, err)
		decoded := ktavIntegrityWrite(t, ktavPath, bogus)
		require.ErrorIs(t, modelsIntegrityOf(decoded, false), ErrModelsEditedDirectly)
		s := ktavIntegrityStore(t, ktavPath)
		s.verifyModelsIntegrityAtLoad()
		require.ErrorIs(t, s.ModelsIntegrity(), ErrModelsEditedDirectly)
	})

	t.Run("non-string signature", func(t *testing.T) {
		ktavPath := filepath.Join(t.TempDir(), "rush.ktav")
		doc := integrityModelsFixture(t, ktavPath, `{"smart":{"provider":"p","model":"m"}}`)
		numeric, err := sjson.Set(doc, "cache_rev", 123)
		require.NoError(t, err)
		decoded := ktavIntegrityWrite(t, ktavPath, numeric)
		require.ErrorIs(t, modelsIntegrityOf(decoded, false), ErrModelsEditedDirectly)
	})

	t.Run("undecodable document fails closed", func(t *testing.T) {
		ktavPath := filepath.Join(t.TempDir(), "rush.ktav")
		require.NoError(t, os.WriteFile(ktavPath, []byte("k: not a ktav document\nk: not a ktav document\n"), 0o600))
		s := ktavIntegrityStore(t, ktavPath)
		s.verifyModelsIntegrityAtLoad()
		require.ErrorIs(t, s.ModelsIntegrity(), ErrModelsEditedDirectly)
	})
}

// TestModelsIntegrityKtav_MigrationRecomputesValidSignature pins the 1.50 ->
// 1.5 migration hazard: a signature computed over the JSON lexemes (1.50)
// would NOT verify after Ktav normalizes the lexeme (1.5), so
// applyKtavMigrationModelsIntegrity must recompute it over the models value
// AS IT READS BACK from the Ktav document. The migrated .ktav must verify
// VALID via modelsIntegrityOf on its decoded bytes.
func TestModelsIntegrityKtav_MigrationRecomputesValidSignature(t *testing.T) {
	integrityIsolate(t)
	source := integrityModelsFixture(t, "rush.json", `{"smart":{"provider":"p","model":"m","temperature":1.50}}`)
	// Sanity for the hazard: the two canonical forms are distinct lexically.
	canonicalSource, present, err := canonicalModelsJSON(gjson.Get(source, "models").Raw)
	require.NoError(t, err)
	require.True(t, present)

	// Sign over the JSON document, convert to Ktav, run the migration
	// cache_rev step exactly as runKtavMigrationTransaction does.
	signed, err := applyModelsIntegrity(source)
	require.NoError(t, err)
	converted, err := ktavcodec.ConvertJSONToKtav([]byte(signed))
	require.NoError(t, err)
	migrated, err := applyKtavMigrationModelsIntegrity([]byte(signed), converted)
	require.NoError(t, err)

	decoded := mustDecodeTestKtav(t, migrated)
	canonicalDecoded, present, err := canonicalModelsJSON(gjson.GetBytes(decoded, "models").Raw)
	require.NoError(t, err)
	require.True(t, present)
	require.NotEqual(t, canonicalSource, canonicalDecoded,
		"the fixture must actually exercise the 1.50 -> 1.5 lexeme normalization")

	require.Equal(t, modelsIntegritySignature(canonicalDecoded), gjson.GetBytes(decoded, "cache_rev").String(),
		"cache_rev must be recomputed over the Ktav-normalized models")
	require.NoError(t, modelsIntegrityOf(decoded, false), "the migrated .ktav must verify VALID")
	require.NoError(t, modelsIntegrityOf(decoded, true))
}

// TestModelsIntegrityKtav_MigrationPreservesMismatchAndMissing pins the other
// two classifications of applyKtavMigrationModelsIntegrity: a TAMPERED
// cache_rev (tampered before migration) is copied through UNCHANGED and stays
// a mismatch, and a MISSING cache_rev stays missing; sync_rev is untouched.
func TestModelsIntegrityKtav_MigrationPreservesMismatchAndMissing(t *testing.T) {
	integrityIsolate(t)
	models := `{"smart":{"provider":"p","model":"m","temperature":1.50}}`

	t.Run("tampered cache_rev stays a mismatch", func(t *testing.T) {
		tampered := integrityModelsFixture(t, "rush.json", models)
		var err error
		tampered, err = sjson.Set(tampered, "cache_rev", "deadbeef")
		require.NoError(t, err)
		tampered, err = sjson.Set(tampered, "sync_rev", HashPassword("pw"))
		require.NoError(t, err)
		converted, err := ktavcodec.ConvertJSONToKtav([]byte(tampered))
		require.NoError(t, err)
		migrated, err := applyKtavMigrationModelsIntegrity([]byte(tampered), converted)
		require.NoError(t, err)

		decoded := mustDecodeTestKtav(t, migrated)
		require.Equal(t, "deadbeef", gjson.GetBytes(decoded, "cache_rev").String(),
			"the tampered value must never be recomputed (blessed) by the migration")
		require.Equal(t, HashPassword("pw"), gjson.GetBytes(decoded, "sync_rev").String(),
			"sync_rev must be untouched by the migration integrity step")
		require.ErrorIs(t, modelsIntegrityOf(decoded, false), ErrModelsEditedDirectly)
	})

	t.Run("missing cache_rev stays missing", func(t *testing.T) {
		source := integrityModelsFixture(t, "rush.json", models)
		converted, err := ktavcodec.ConvertJSONToKtav([]byte(source))
		require.NoError(t, err)
		migrated, err := applyKtavMigrationModelsIntegrity([]byte(source), converted)
		require.NoError(t, err)
		decoded := mustDecodeTestKtav(t, migrated)
		require.False(t, gjson.GetBytes(decoded, "cache_rev").Exists())
		require.NoError(t, modelsIntegrityOf(decoded, false))
	})
}
