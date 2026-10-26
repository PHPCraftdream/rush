// --global-dir isolation regression: with the root active AND decoy
// "would-be-real" global dirs staged behind the RUSH_GLOBAL_* / XDG_* /
// HOME overrides, a real config Load, a real global settings write, and a
// real OAuth journal write must leave every decoy byte-identical. Catches
// any code path that auto-imports, ktav-migrates, or rotates files outside
// the isolated root.
package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestGlobalDirIsolationNoFallbackImports(t *testing.T) {
	// --- stage decoy "real" global dirs behind the override env vars ---
	base := t.TempDir()
	home := filepath.Join(base, "home")
	cfgDir := filepath.Join(base, "xdg-config")
	dataDir := filepath.Join(base, "xdg-data")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", cfgDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("RUSH_CACHE_DIR", filepath.Join(base, "cache"))
	t.Setenv(GlobalDirEnv, filepath.Join(t.TempDir(), "root"))

	decoys := map[string][]byte{}
	writeDecoy := func(path string, content []byte) {
		t.Helper()
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, content, 0o600))
		decoys[path] = content
	}

	// Decoy global config slot (legacy rush.json + rush.ktav sibling) at
	// the would-be-real config location.
	decoyConfigJSON := filepath.Join(cfgDir, "rush.json")
	decoyConfigData := writeIntegrityFixture(t, decoyConfigJSON, "")
	decoys[decoyConfigJSON] = decoyConfigData
	decoyConfigKtav := filepath.Join(cfgDir, "rush.ktav")
	writeDecoy(decoyConfigKtav, testKtavBytes(t, integrityModelsFixture(t, decoyConfigKtav, "")))

	// Decoy global data slot (same pair) at the would-be-real data
	// location — this is also where the platform-canonical lock seam is
	// pointed below.
	decoyDataJSON := filepath.Join(dataDir, "rush.json")
	writeIntegrityFixture(t, decoyDataJSON, "")
	decoyDataKtav := filepath.Join(dataDir, "rush.ktav")
	writeDecoy(decoyDataKtav, testKtavBytes(t, integrityModelsFixture(t, decoyDataKtav, "")))

	// Decoy OAuth pending journal (legacy JSON and Ktav spellings) at the
	// would-be-real data dir.
	journalHash := oauthJournalDigest("anthropic-claude")
	decoyJournal := filepath.Join(dataDir,
		"rush.json.oauth-pending-"+journalHash+".json")
	decoyJournalKtav := filepath.Join(dataDir,
		"rush.ktav.oauth-pending-"+journalHash+".ktav")
	writeDecoy(decoyJournal, []byte(`{"token":{"access_token":"decoy","refresh_token":"decoy","expires_at":1}}`))
	writeDecoy(decoyJournalKtav, testKtavBytes(t, `{"token":{"access_token":"decoy","refresh_token":"decoy","expires_at":1}}`))

	// Decoy provider cache at the would-be-real cache slot.
	decoyProviders := filepath.Join(dataDir, "providers.json")
	writeDecoy(decoyProviders, []byte(`{"providers":{}}`))

	// Platform-canonical seam → the decoy data path, exactly like the
	// settings-lock tests replace it. The decoy carries NO lock, so a
	// correct implementation may READ it (hash lookup) but must never
	// write, convert, or import it.
	previous := canonicalGlobalDataPath
	canonicalGlobalDataPath = func() string { return decoyDataJSON }
	t.Cleanup(func() { canonicalGlobalDataPath = previous })

	// No process password → no locks anywhere → writes must be allowed.
	SetProcessPassword("")
	t.Cleanup(func() { SetProcessPassword("") })

	// --- real config Load ---
	workingDir := t.TempDir()
	store, err := Load(workingDir, "", false)
	require.NoError(t, err)
	require.NotNil(t, store)

	// The loaded global slot must be the isolated one, not the decoys.
	require.Equal(t,
		normalizeReloadPath(filepath.Join(os.Getenv(GlobalDirEnv), "data", "rush.json")),
		normalizeReloadPath(GlobalConfigData()))

	// --- real global settings write ---
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.debug", true))

	// --- real OAuth journal write through the production disk layer ---
	journalPath := selectOAuthJournalPath(store.globalDataPath, "anthropic-claude", true)
	// Canonical forms: Windows temp dirs may come back as 8.3 short names.
	require.Equal(t,
		normalizeReloadPath(filepath.Join(os.Getenv(GlobalDirEnv), "data")),
		normalizeReloadPath(filepath.Dir(journalPath)),
		"journal must be written inside the isolated root")
	require.NoError(t, writeOAuthJournalUntilDurable(context.Background(), journalPath,
		&oauthJournalRecord{Token: &oauth.Token{
			AccessToken:  "access-token",
			RefreshToken: "refresh-token",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}}))
	_, err = os.Stat(journalPath)
	require.NoError(t, err, "journal write must have landed in the root")

	// --- every decoy is byte-identical ---
	for path, want := range decoys {
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, want, got, "decoy file was modified: %s", path)
	}
}
