package cmd

// KTAV-4b chunk 3: `rush migrate` (crush.json -> rush config) must produce
// the format config's newConfigFileIsKtav() gate selects. Gate off: rush.json
// exactly as before (byte-identical behavior). Gate on: rush.ktav, written
// through the ktavcodec document layer, with no rush.json left behind and
// conflict detection covering BOTH spellings of the destination slot.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migrateKtavFixtureJSON is a valid rush settings document: one custom
// provider (base_url + models, the shape config.Load requires) plus an
// options.disabled_skills entry carrying a legacy skill ID so the migration's
// legacy-content rewrite has real work to do in every test below.
const migrateKtavFixtureJSON = `{
  "providers": {
    "ktav-mig-p": {
      "name": "Ktav Mig P",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.example.com/v1",
      "models": [{"id": "gpt-4o"}]
    }
  },
  "options": {
    "disabled_skills": ["crush-config"]
  }
}`

// isolateMigrateKtavEnv redirects every global-location environment variable
// migrate consults to temp directories (same isolation contract as
// migrate_test.go's isolateGlobalPaths, plus the home variables
// legacyGlobalConfigPath/legacyGlobalDataPath can fall back to), and returns
// the canonicalized project root.
func isolateMigrateKtavEnv(t *testing.T) (workspace string, configDir, dataDir string) {
	t.Helper()
	configDir, dataDir = isolateGlobalPaths(t)
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("CRUSH_GLOBAL_CONFIG", "")
	t.Setenv("CRUSH_GLOBAL_DATA", "")

	workspace = t.TempDir()
	canonical, err := filepath.EvalSymlinks(workspace)
	require.NoError(t, err)
	return canonical, configDir, dataDir
}

// runMigrateKtav runs the migrate command against workspace, capturing output.
func runMigrateKtav(t *testing.T, workspace string) (string, error) {
	t.Helper()
	var b bytes.Buffer
	migrateCmd.SetOut(&b)
	migrateCmd.SetErr(&b)
	migrateCmd.SetIn(bytes.NewReader(nil))
	err := migrateCmd.RunE(migrateCmd, []string{workspace})
	return b.String(), err
}

// seedMigrateKtavProject writes a legacy crush.json fixture into workspace.
func seedMigrateKtavProject(t *testing.T, workspace string) string {
	t.Helper()
	crushFile := filepath.Join(workspace, "crush.json")
	require.NoError(t, os.WriteFile(crushFile, []byte(migrateKtavFixtureJSON), 0o644))
	return crushFile
}

// requireMigratedProviderSemantics asserts the decoded document still carries
// the fixture's custom provider and that the legacy disabled_skills entry was
// rewritten to its rush-named builtin ID.
func requireMigratedProviderSemantics(t *testing.T, docBytes []byte) {
	t.Helper()
	var doc struct {
		Providers map[string]struct {
			BaseURL string `json:"base_url"`
		} `json:"providers"`
		Options struct {
			DisabledSkills []string `json:"disabled_skills"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(docBytes, &doc))
	provider, ok := doc.Providers["ktav-mig-p"]
	require.True(t, ok, "custom provider must survive migration")
	assert.Equal(t, "https://api.example.com/v1", provider.BaseURL)
	assert.Equal(t, []string{"rush-config"}, doc.Options.DisabledSkills,
		"legacy disabled_skills entry must be rewritten to the rush-named builtin ID")
}

// TestMigrateKtavGateOffWritesRushJSON pins gate-off behavior: crush.json
// migrates to rush.json (never rush.ktav), the produced content is the
// migrated JSON (legacy values rewritten in place), and the crush.json input
// is gone. Every byte of the gate-off path must behave as before this change.
//
// First assertion trace: the fixture's disabled_skills is ["crush-config"],
// which legacySkillRenames maps to "rush-config", so the rewrite marks the
// document changed and rewrites it; the test's first meaningful assertion is
// that rush.json exists on disk at all, then that its decoded semantics show
// the rewritten skill ID.
func TestMigrateKtavGateOffWritesRushJSON(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	workspace, _, _ := isolateMigrateKtavEnv(t)
	crushFile := seedMigrateKtavProject(t, workspace)

	_, err := runMigrateKtav(t, workspace)
	require.NoError(t, err)

	rushJSON := filepath.Join(workspace, "rush.json")
	raw, err := os.ReadFile(rushJSON)
	require.NoError(t, err, "gate off must produce rush.json")

	_, statErr := os.Stat(crushFile)
	assert.True(t, os.IsNotExist(statErr), "crush.json input must be gone")
	_, ktavErr := os.Stat(filepath.Join(workspace, "rush.ktav"))
	assert.True(t, os.IsNotExist(ktavErr), "gate off must never produce rush.ktav")

	requireMigratedProviderSemantics(t, raw)
}

// TestMigrateKtavGateOnWritesRushKtav pins gate-on behavior: crush.json
// migrates to rush.ktav, the file parses as Ktav and ToJSON round-trips to
// the migrated JSON semantics, and NO rush.json artifact of the migration
// (nor the crush.json input) remains.
//
// First assertion trace: the same fixture as the gate-off test evaluates to a
// document whose disabled_skills ["crush-config"] rewrites to "rush-config";
// after the rename to rush.ktav, rewriteLegacyConfigContent is forced through
// ktavcodec.ConvertJSONToKtav, so the first assertion — rush.ktav exists —
// must hold, and DecodeKtav must succeed on its bytes.
func TestMigrateKtavGateOnWritesRushKtav(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	workspace, _, _ := isolateMigrateKtavEnv(t)
	crushFile := seedMigrateKtavProject(t, workspace)

	_, err := runMigrateKtav(t, workspace)
	require.NoError(t, err)

	ktavPath := filepath.Join(workspace, "rush.ktav")
	raw, err := os.ReadFile(ktavPath)
	require.NoError(t, err, "gate on must produce rush.ktav")

	_, statErr := os.Stat(filepath.Join(workspace, "rush.json"))
	assert.True(t, os.IsNotExist(statErr), "no rush.json artifact of the migration may remain")
	_, statErr = os.Stat(crushFile)
	assert.True(t, os.IsNotExist(statErr), "no stale crush.json may remain")

	tree, err := ktavcodec.DecodeKtav(raw)
	require.NoError(t, err, "produced rush.ktav must parse as Ktav")
	jsonBytes, err := ktavcodec.ToJSON(tree)
	require.NoError(t, err)
	requireMigratedProviderSemantics(t, jsonBytes)
}

// TestMigrateKtavGateOnConflictExistingKtav pins that, gate on, an existing
// rush.ktav in the target directory is a conflict: the migration is refused
// (same error surface as a rush.json conflict), and neither the existing
// rush.ktav nor the crush.json input is touched.
func TestMigrateKtavGateOnConflictExistingKtav(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	workspace, _, _ := isolateMigrateKtavEnv(t)
	crushFile := seedMigrateKtavProject(t, workspace)

	existingKtav, err := ktavcodec.ConvertJSONToKtav([]byte(`{"providers":{}}`))
	require.NoError(t, err)
	ktavPath := filepath.Join(workspace, "rush.ktav")
	require.NoError(t, os.WriteFile(ktavPath, existingKtav, 0o644))

	out, err := runMigrateKtav(t, workspace)
	require.Error(t, err, "an existing rush.ktav in the target must be refused")
	assert.Contains(t, err.Error(), "refused due to conflicts")
	assert.Contains(t, out, "CONFLICT")
	assert.Contains(t, out, ktavPath)

	got, readErr := os.ReadFile(ktavPath)
	require.NoError(t, readErr)
	assert.Equal(t, string(existingKtav), string(got), "existing rush.ktav must not be clobbered")
	_, statErr := os.Stat(crushFile)
	assert.NoError(t, statErr, "conflict must leave crush.json untouched")
}

// TestMigrateKtavGateOnConflictExistingRushJSON pins that, gate on, an
// existing rush.json in the target directory is still a conflict — the
// gate-on Ktav destination must never clobber or shadow a legacy rush.json.
func TestMigrateKtavGateOnConflictExistingRushJSON(t *testing.T) {
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	workspace, _, _ := isolateMigrateKtavEnv(t)
	crushFile := seedMigrateKtavProject(t, workspace)

	rushJSON := filepath.Join(workspace, "rush.json")
	require.NoError(t, os.WriteFile(rushJSON, []byte(`{"providers":{}}`), 0o644))

	out, err := runMigrateKtav(t, workspace)
	require.Error(t, err, "an existing rush.json in the target must be refused")
	assert.Contains(t, err.Error(), "refused due to conflicts")
	assert.Contains(t, out, "CONFLICT")
	assert.Contains(t, out, rushJSON)

	_, statErr := os.Stat(filepath.Join(workspace, "rush.ktav"))
	assert.True(t, os.IsNotExist(statErr), "conflict must not create rush.ktav")
	got, readErr := os.ReadFile(rushJSON)
	require.NoError(t, readErr)
	assert.Equal(t, `{"providers":{}}`, string(got), "existing rush.json must not be clobbered")
}
