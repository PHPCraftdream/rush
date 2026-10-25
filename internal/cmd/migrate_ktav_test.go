package cmd

// KTAV-4b CLI repair round 1, chunk A: `rush migrate` (crush.json -> rush
// config) must produce the format config's KtavAutoMigrateEnabled() gate
// selects, and the gate-on real run must ALWAYS emit Ktav bytes at
// rush.ktav - including when the legacy document has no `options` member at
// all (provider-only configs), which is the data-corrupting early-return
// this round fixes: previously rewriteLegacyConfigContent returned
// statusNone before the forced conversion, leaving JSON bytes inside a
// .ktav file with no fallback copy.

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
// legacy-content rewrite has real work to do.
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

// migrateKtavProviderOnlyJSON is the P1 repro shape: a valid provider
// document with NO `options` member at all. Gate on, this must still convert
// to Ktav bytes at rush.ktav - the old early-return-on-no-options path left
// JSON inside the .ktav spelling.
const migrateKtavProviderOnlyJSON = `{
  "providers": {
    "ktav-mig-p": {
      "name": "Ktav Mig P",
      "type": "openai",
      "api_key": "sk-1234567890abcdef",
      "base_url": "https://api.example.com/v1",
      "models": [{"id": "gpt-4o"}]
    }
  }
}`

// migrateKtavNoRewriteJSON has an options member but nothing legacy to
// rewrite - the other early-return case class that must still convert.
const migrateKtavNoRewriteJSON = `{
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
    "debug": true
  }
}`

// isolateMigrateKtavEnv isolates every global-location environment variable
// migrate consults (ktavIsolateGlobalEnv + isolateGlobalPaths, plus the
// home/appdata fallbacks legacyGlobalConfigPath/legacyGlobalDataPath can
// reach), and returns the canonicalized project root.
func isolateMigrateKtavEnv(t *testing.T) string {
	t.Helper()
	ktavIsolateGlobalEnv(t)
	isolateGlobalPaths(t)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("CRUSH_GLOBAL_CONFIG", "")
	t.Setenv("CRUSH_GLOBAL_DATA", "")

	workspace := t.TempDir()
	canonical, err := filepath.EvalSymlinks(workspace)
	require.NoError(t, err)
	return canonical
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
func seedMigrateKtavProject(t *testing.T, workspace, content string) string {
	t.Helper()
	crushFile := filepath.Join(workspace, "crush.json")
	require.NoError(t, os.WriteFile(crushFile, []byte(content), 0o644))
	return crushFile
}

// requireMigratedProviderSemantics asserts the decoded JSON document still
// carries the fixture's custom provider.
func requireMigratedProviderSemantics(t *testing.T, docBytes []byte) {
	t.Helper()
	var doc struct {
		Providers map[string]struct {
			BaseURL string `json:"base_url"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(docBytes, &doc))
	provider, ok := doc.Providers["ktav-mig-p"]
	require.True(t, ok, "custom provider must survive migration")
	assert.Equal(t, "https://api.example.com/v1", provider.BaseURL)
}

// decodeKtavToJSON decodes Ktav bytes back to their JSON semantics.
func decodeKtavToJSON(t *testing.T, raw []byte) []byte {
	t.Helper()
	tree, err := ktavcodec.DecodeKtav(raw)
	require.NoError(t, err, "produced rush.ktav must parse as Ktav")
	jsonBytes, err := ktavcodec.ToJSON(tree)
	require.NoError(t, err)
	return jsonBytes
}

// requireGateOnKtavMigration runs a gate-on migration of content and asserts
// the full success contract: rush.ktav carries the converted document, no
// rush.json is produced, and crush.json is gone. Returns the decoded JSON.
func requireGateOnKtavMigration(t *testing.T, workspace, content string) []byte {
	t.Helper()
	crushFile := seedMigrateKtavProject(t, workspace, content)

	_, err := runMigrateKtav(t, workspace)
	require.NoError(t, err)

	ktavPath := filepath.Join(workspace, "rush.ktav")
	raw, err := os.ReadFile(ktavPath)
	require.NoError(t, err, "gate on must produce rush.ktav")

	_, statErr := os.Stat(filepath.Join(workspace, "rush.json"))
	assert.True(t, os.IsNotExist(statErr), "no rush.json artifact of the migration may remain")
	_, statErr = os.Stat(crushFile)
	assert.True(t, os.IsNotExist(statErr), "no stale crush.json may remain")

	return decodeKtavToJSON(t, raw)
}

// TestMigrateKtavGateOffWritesRushJSON pins gate-off behavior: crush.json
// migrates to rush.json (never rush.ktav), the produced content is the
// migrated JSON (legacy values rewritten in place), and the crush.json input
// is gone.
//
// First assertion trace: migrateFile derives newFile=rush.json (gate off),
// renames, then rewriteLegacyConfigContent's applyLegacyRewrites finds
// disabled_skills ["crush-config"] -> "rush-config" (legacySkillRenames), so
// changed=true and the rewritten JSON is written atomically. The test's
// first meaningful assertion is that rush.json exists at all.
func TestMigrateKtavGateOffWritesRushJSON(t *testing.T) {
	ktavIsolateGlobalEnv(t)
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	workspace := isolateMigrateKtavEnv(t)
	crushFile := seedMigrateKtavProject(t, workspace, migrateKtavFixtureJSON)

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

	var doc struct {
		Options struct {
			DisabledSkills []string `json:"disabled_skills"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(raw, &doc))
	assert.Equal(t, []string{"rush-config"}, doc.Options.DisabledSkills,
		"legacy disabled_skills entry must be rewritten to the rush-named builtin ID")
}

// TestMigrateKtavGateOffConflictExistingKtav pins the both-spelling conflict
// rule at gate off: an existing rush.ktav in the target directory must
// refuse the migration even though the gate would produce rush.json —
// writing rush.json beside rush.ktav would create a duplicate config the
// app (which selects exactly one slot) would silently ignore.
//
// First assertion trace: migrateExistingInnerConfig finds rush.ktav (its
// second Stat) before any rename, so migrateFile returns statusRefused and
// runMigrate returns the "refused due to conflicts" error — the first
// meaningful assertion is require.Error.
func TestMigrateKtavGateOffConflictExistingKtav(t *testing.T) {
	ktavIsolateGlobalEnv(t)
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	workspace := isolateMigrateKtavEnv(t)
	crushFile := seedMigrateKtavProject(t, workspace, migrateKtavFixtureJSON)

	existingKtav, err := ktavcodec.ConvertJSONToKtav([]byte(`{"providers":{}}`))
	require.NoError(t, err)
	ktavPath := filepath.Join(workspace, "rush.ktav")
	require.NoError(t, os.WriteFile(ktavPath, existingKtav, 0o644))

	out, err := runMigrateKtav(t, workspace)
	require.Error(t, err, "an existing rush.ktav in the target must be refused even at gate off")
	assert.Contains(t, err.Error(), "refused due to conflicts")
	assert.Contains(t, out, "CONFLICT")
	assert.Contains(t, out, ktavPath)

	got, readErr := os.ReadFile(ktavPath)
	require.NoError(t, readErr)
	assert.Equal(t, string(existingKtav), string(got), "existing rush.ktav must not be clobbered")
	_, statErr := os.Stat(crushFile)
	assert.NoError(t, statErr, "conflict must leave crush.json untouched")
}

// TestMigrateKtavGateOnProviderOnlyConverts is the P1 repro: gate on, a
// provider-only crush.json with NO options member must still be converted
// to Ktav bytes at rush.ktav. The pre-fix code returned statusNone on the
// missing-options early path, leaving JSON inside a .ktav file with no
// fallback copy.
//
// First assertion trace: migrateFile (gate on, real run) calls
// migrateToKtavTarget, which always runs ConvertJSONToKtav regardless of the
// changed flag — so rush.ktav exists and DecodeKtav succeeds; the first
// meaningful assertion is require.NoError on reading rush.ktav.
func TestMigrateKtavGateOnProviderOnlyConverts(t *testing.T) {
	ktavIsolateGlobalEnv(t)
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	workspace := isolateMigrateKtavEnv(t)
	jsonBytes := requireGateOnKtavMigration(t, workspace, migrateKtavProviderOnlyJSON)
	requireMigratedProviderSemantics(t, jsonBytes)
}

// TestMigrateKtavGateOnRewritesLegacyValues pins that the gate-on conversion
// carries the legacy-value rewrites: disabled_skills ["crush-config"] must
// decode back as ["rush-config"] from the produced rush.ktav.
//
// First assertion trace: migrateToKtavTarget's applyLegacyRewrites maps
// "crush-config" through legacySkillRenames and reports a note; the test's
// first meaningful assertion is rush.ktav's existence (require.NoError on
// ReadFile), then the decoded skill list.
func TestMigrateKtavGateOnRewritesLegacyValues(t *testing.T) {
	ktavIsolateGlobalEnv(t)
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	workspace := isolateMigrateKtavEnv(t)
	jsonBytes := requireGateOnKtavMigration(t, workspace, migrateKtavFixtureJSON)

	var doc struct {
		Options struct {
			DisabledSkills []string `json:"disabled_skills"`
		} `json:"options"`
	}
	require.NoError(t, json.Unmarshal(jsonBytes, &doc))
	assert.Equal(t, []string{"rush-config"}, doc.Options.DisabledSkills,
		"legacy disabled_skills entry must be rewritten to the rush-named builtin ID")
}

// TestMigrateKtavGateOnNoRewriteStillConverts pins the early-return case
// class end to end: gate on, an options member with nothing legacy to
// rewrite must STILL produce a Ktav document whose decoded JSON semantics
// exactly match the source.
//
// First assertion trace: migrateToKtavTarget ignores the changed=false
// result and converts anyway — rush.ktav exists and decodes; the first
// meaningful assertion is require.NoError on reading rush.ktav.
func TestMigrateKtavGateOnNoRewriteStillConverts(t *testing.T) {
	ktavIsolateGlobalEnv(t)
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	workspace := isolateMigrateKtavEnv(t)
	jsonBytes := requireGateOnKtavMigration(t, workspace, migrateKtavNoRewriteJSON)

	var got, want map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(jsonBytes, &got))
	require.NoError(t, json.Unmarshal([]byte(migrateKtavNoRewriteJSON), &want))
	assert.Equal(t, want, got, "nothing-to-rewrite document must round-trip semantics exactly")
}

// TestMigrateKtavGateOnConflictExistingKtav pins that, gate on, an existing
// rush.ktav in the target directory is a conflict: the migration is refused
// (same error surface as a rush.json conflict), and neither the existing
// rush.ktav nor the crush.json input is touched.
//
// First assertion trace: migrateExistingInnerConfig's rush.json Stat misses,
// its rush.ktav Stat hits, so migrateFile returns statusRefused before
// migrateToKtavTarget runs — the first meaningful assertion is
// require.Error.
func TestMigrateKtavGateOnConflictExistingKtav(t *testing.T) {
	ktavIsolateGlobalEnv(t)
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	workspace := isolateMigrateKtavEnv(t)
	crushFile := seedMigrateKtavProject(t, workspace, migrateKtavFixtureJSON)

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
//
// First assertion trace: migrateExistingInnerConfig's first Stat (rush.json)
// hits, so migrateFile returns statusRefused and no rush.ktav is ever
// created — the first meaningful assertion is require.Error.
func TestMigrateKtavGateOnConflictExistingRushJSON(t *testing.T) {
	ktavIsolateGlobalEnv(t)
	restore := config.SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)

	workspace := isolateMigrateKtavEnv(t)
	crushFile := seedMigrateKtavProject(t, workspace, migrateKtavFixtureJSON)

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
	_, statErr = os.Stat(crushFile)
	assert.NoError(t, statErr, "conflict must leave crush.json untouched")
}
