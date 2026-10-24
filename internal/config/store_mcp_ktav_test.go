// Ktav MCP transaction tests: the Rush-owned `mcp` records inside a
// rush.ktav document are edited through the same JSON-shaped scanner and
// transaction machinery as rush.json — Ktav is only the DISK representation.
// The external .mcp.json (`mcpServers`) document stays plain JSON and its
// bytes are never touched by Rush-scope transactions.
//
// Revert-checks (orchestrator runs the mutants):
//   - TestMCPKtavRushDocumentRoundTrip -> skipping the decode layer in
//     withMCPLocksUsingCaseSensitivity (store_mcp_locks.go) or the
//     encodeSettingsDocument step in writeMCPFileChanges
//     (store_mcp_transaction.go) makes the transaction fail on, or corrupt,
//     a rush.ktav target.
//   - TestMCPKtavCorruptDocumentAbortsAndHidesSecret -> mapping a decode
//     failure to "no servers"/absent (store_mcp_locks.go decode / mcpPathData)
//     makes the mutation succeed and rewrite the document, or leak the
//     secret into the error message.
//   - TestMCPKtavStaleRawFingerprintABARefusal -> committing against a
//     fingerprint computed from the DECODED bytes instead of the RAW bytes
//     lets the stale transaction overwrite the concurrent writer.
//   - TestMCPKtavRevisionsTrackRawBytes -> dropping the raw-digest mixing
//     for Ktav documents in mcpInputFingerprints (mcp_revisions.go) keeps
//     the revision frozen across a raw-bytes-only rewrite.
//   - TestMCPKtavLockAnchorMatchesLegacySidecar -> anchoring the write
//     sidecar on the .ktav spelling (configWriteLockAnchor) splits
//     rush.ktav and rush.json onto two sidecars.
package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// mcpKtavIsolate redirects every global-config/home environment variable to
// subdirectories of one symlink-resolved temp root, disables provider
// discovery, and returns the root and the canonical global directories.
func mcpKtavIsolate(t *testing.T) (root, configDir, dataDir string) {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root = tmp
	configDir = filepath.Join(root, "global-config")
	dataDir = filepath.Join(root, "global-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("RUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "home"), 0o700))
	return root, configDir, dataDir
}

// mcpKtavFixture builds a valid rush document body: a custom provider
// (base_url + models, required for Load), the MCP container, and an unknown
// future top-level field the mutations must preserve.
func mcpKtavFixture(mcpJSON, apiKey string) string {
	return `{"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"` + apiKey + `","models":[{"id":"model","name":"Model"}]}},"mcp":` + mcpJSON + `,"future_unknown_field":{"nested":[1,2,"three"]}}`
}

const mcpKtavSecret = "sk-SUPERSECRET-mcp-42"

// mcpKtavDiskJSON reads path and requires it to still decode as Ktav,
// returning its JSON-shaped bytes.
func mcpKtavDiskJSON(t *testing.T, path string) []byte {
	t.Helper()
	return mustDecodeTestKtav(t, fileBytes(t, path))
}

// mcpKtavRawString unquotes a JSON raw-message scalar.
func mcpKtavRawString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	require.NoError(t, json.Unmarshal(raw, &value))
	return value
}

// mcpKtavDecodedSeed decodes fixture JSON through the Ktav codec so
// expectations are compared in the same canonical spelling the disk
// document decodes to.
func mcpKtavDecodedSeed(t *testing.T, fixture string) map[string]json.RawMessage {
	t.Helper()
	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(mustDecodeTestKtav(t, testKtavBytes(t, fixture)), &root))
	return root
}

// TestMCPKtavRushDocumentRoundTrip drives add/update/disable/remove of a
// Rush MCP server stored in rush.ktav: every commit lands back in the SAME
// file as valid Ktav with the expected JSON-shaped content, unrelated keys
// and unknown fields survive, and no sibling rush.json is ever created.
func TestMCPKtavRushDocumentRoundTrip(t *testing.T) {
	root, _, dataDir := mcpKtavIsolate(t)
	ktavPath := filepath.Join(dataDir, "rush.ktav")
	legacyPath := filepath.Join(dataDir, "rush.json")
	name := "ktav.srv#one"
	seed := mcpKtavFixture(`{"`+name+`":{"type":"http","url":"http://seed.example"}}`, mcpKtavSecret)
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, seed), 0o600))
	seedRoot := mcpKtavDecodedSeed(t, seed)

	store, err := Init(root, root, false)
	require.NoError(t, err)

	assertUnrelatedPreserved := func(doc []byte) {
		t.Helper()
		var current map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(doc, &current))
		require.JSONEq(t, string(seedRoot["providers"]), string(current["providers"]),
			"the provider block must survive the round trip semantically")
		require.JSONEq(t, string(seedRoot["future_unknown_field"]), string(current["future_unknown_field"]),
			"unknown top-level fields must survive the round trip")
	}

	// Add.
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "added", MCPConfig{
		Type: MCPHttp, URL: "http://added.example",
	}))
	doc := mcpKtavDiskJSON(t, ktavPath)
	entry, ok := mcpEntryFromJSON(doc, "added")
	require.True(t, ok)
	require.Equal(t, "http://added.example", mcpKtavRawString(t, entry["url"]))
	seedEntry, ok := mcpEntryFromJSON(doc, name)
	require.True(t, ok, "pre-existing entry must survive")
	require.NotNil(t, seedEntry["type"])
	assertUnrelatedPreserved(doc)
	require.NoFileExists(t, legacyPath, "a rush.ktav store must never grow a rush.json sibling")

	// Update.
	require.NoError(t, store.PersistMCPFields(ScopeGlobal, "added", map[string]any{
		"url": "http://updated.example",
	}))
	doc = mcpKtavDiskJSON(t, ktavPath)
	entry, ok = mcpEntryFromJSON(doc, "added")
	require.True(t, ok)
	require.Equal(t, "http://updated.example", mcpKtavRawString(t, entry["url"]))
	_, ok = mcpEntryFromJSON(doc, name)
	require.True(t, ok)
	assertUnrelatedPreserved(doc)
	require.NoFileExists(t, legacyPath)

	// Disable the Rush-owned server in place (full definition preserved).
	require.NoError(t, store.PersistMCPDisabledOverride(ScopeGlobal, name, true))
	doc = mcpKtavDiskJSON(t, ktavPath)
	entry, ok = mcpEntryFromJSON(doc, name)
	require.True(t, ok)
	require.Equal(t, "true", string(entry["disabled"]))
	require.NotNil(t, entry["type"], "disable must keep the full definition")
	assertUnrelatedPreserved(doc)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	disabled, exists := store.MCPConfig(name)
	require.True(t, exists)
	require.True(t, disabled.Disabled)

	// Remove.
	require.NoError(t, store.PersistRemoveMCPConfig(ScopeGlobal, "added"))
	doc = mcpKtavDiskJSON(t, ktavPath)
	_, ok = mcpEntryFromJSON(doc, "added")
	require.False(t, ok)
	_, ok = mcpEntryFromJSON(doc, name)
	require.True(t, ok, "remove must only remove the named entry")
	assertUnrelatedPreserved(doc)
	require.NoFileExists(t, legacyPath)
}

// TestMCPKtavMixedScopesKeepOrderingAndExternalBytes pins scope ordering
// across a global rush.ktav, a project rush.json and an external .mcp.json:
// workspace beats global for Rush definitions, the external file's bytes are
// untouched by every Rush-scope transaction, and its disabled state is an
// overlay in the Rush store only.
func TestMCPKtavMixedScopesKeepOrderingAndExternalBytes(t *testing.T) {
	root, _, dataDir := mcpKtavIsolate(t)
	ktavPath := filepath.Join(dataDir, "rush.ktav")
	workspacePath := filepath.Join(root, "rush.json")
	externalPath := filepath.Join(root, ".mcp.json")

	seedKtav := testKtavBytes(t, mcpKtavFixture(`{"dup":{"type":"http","url":"http://global.example"}}`, mcpKtavSecret))
	require.NoError(t, os.WriteFile(ktavPath, seedKtav, 0o600))
	workspaceSeed := []byte(mcpKtavFixture(`{"dup":{"type":"http","url":"http://workspace.example"}}`, "sk-workspace-key"))
	require.NoError(t, os.WriteFile(workspacePath, workspaceSeed, 0o600))
	externalSeed := []byte(`{"mcpServers":{"ext":{"type":"http","url":"http://external.example"}}}`)
	require.NoError(t, os.WriteFile(externalPath, externalSeed, 0o600))

	store, err := Init(root, root, false)
	require.NoError(t, err)
	require.Equal(t, normalizeReloadPath(workspacePath), normalizeReloadPath(store.configPathOrEmpty(ScopeWorkspace)))

	// Workspace Rush definition wins over the global Ktav one; external
	// servers are visible beneath the Rush names.
	dup, exists := store.MCPConfig("dup")
	require.True(t, exists)
	require.Equal(t, "http://workspace.example", dup.URL)
	ext, exists := store.MCPConfig("ext")
	require.True(t, exists)
	require.Equal(t, MCPSourceExternal, ext.Source)

	// A workspace-scope replace must edit ONLY the workspace rush.json.
	require.NoError(t, store.PersistReplaceMCPInScope(ScopeWorkspace, "dup", "renamed", MCPConfig{
		Type: MCPHttp, URL: "http://renamed.example",
	}))
	require.Equal(t, seedKtav, fileBytes(t, ktavPath), "global rush.ktav must be byte-for-byte untouched")
	require.Equal(t, externalSeed, fileBytes(t, externalPath), "external .mcp.json must be byte-for-byte untouched")
	workspaceDoc := fileBytes(t, workspacePath)
	_, ok := mcpEntryFromJSON(workspaceDoc, "renamed")
	require.True(t, ok)
	_, ok = mcpEntryFromJSON(workspaceDoc, "dup")
	require.False(t, ok)

	// The replaced name falls back to the global (Ktav) definition.
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	fallback, exists := store.MCPConfig("dup")
	require.True(t, exists)
	require.Equal(t, "http://global.example", fallback.URL)

	// Disabling the EXTERNAL server writes a disabled-only overlay into the
	// Rush workspace document; the external bytes stay untouched.
	require.NoError(t, store.PersistMCPDisabledOverride(ScopeWorkspace, "ext", true))
	require.Equal(t, seedKtav, fileBytes(t, ktavPath))
	require.Equal(t, externalSeed, fileBytes(t, externalPath))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	ext, exists = store.MCPConfig("ext")
	require.True(t, exists)
	require.True(t, ext.Disabled)
	workspaceDoc = fileBytes(t, workspacePath)
	entry, ok := mcpEntryFromJSON(workspaceDoc, "ext")
	require.True(t, ok, "the overlay must live in the Rush document")
	require.Equal(t, "true", string(entry["disabled"]))
}

// TestMCPKtavStaleRawFingerprintABARefusal injects a concurrent raw rewrite
// of rush.ktav between target resolution and lock-time binding verification:
// the stale transaction must be refused with ErrMCPStale and must not commit
// its own content.
func TestMCPKtavStaleRawFingerprintABARefusal(t *testing.T) {
	_, _, dataDir := mcpKtavIsolate(t)
	ktavPath := filepath.Join(dataDir, "rush.ktav")
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, mcpKtavFixture(`{"srv":{"type":"http","url":"http://seed.example"}}`, mcpKtavSecret)), 0o600))
	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: ktavPath})

	configTestHooks.Lock()
	previous := configTestHooks.beforeMCPTargetBindingVerification
	configTestHooks.beforeMCPTargetBindingVerification = func(target *configWriteTarget) {
		if normalizeReloadPath(target.selectedPath) != normalizeReloadPath(ktavPath) {
			return
		}
		// The "concurrent writer": a raw rewrite with different content.
		concurrent := mcpKtavFixture(`{"srv":{"type":"http","url":"http://seed.example"},"other":{"type":"http","url":"http://other.example"}}`, mcpKtavSecret)
		_ = os.WriteFile(ktavPath, testKtavBytes(t, concurrent), 0o600)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.beforeMCPTargetBindingVerification = previous
		configTestHooks.Unlock()
	})

	err := store.PersistMCPConfig(ScopeGlobal, "aba-target", MCPConfig{
		Type: MCPHttp, URL: "http://aba.example",
	})
	require.ErrorIs(t, err, ErrMCPStale)

	doc := mcpKtavDiskJSON(t, ktavPath)
	_, ok := mcpEntryFromJSON(doc, "other")
	require.True(t, ok, "the concurrent writer's content must survive")
	_, ok = mcpEntryFromJSON(doc, "aba-target")
	require.False(t, ok, "the stale transaction must not commit")
}

// TestMCPKtavCorruptDocumentAbortsAndHidesSecret corrupts rush.ktav after
// the store was built: the transaction must abort with an error that carries
// no document values, and the on-disk bytes must be unchanged.
func TestMCPKtavCorruptDocumentAbortsAndHidesSecret(t *testing.T) {
	_, _, dataDir := mcpKtavIsolate(t)
	ktavPath := filepath.Join(dataDir, "rush.ktav")
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, mcpKtavFixture(`{"srv":{"type":"http","url":"http://seed.example"}}`, mcpKtavSecret)), 0o600))
	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: ktavPath})

	corrupt := []byte("this is definitely not a ktav document")
	require.NoError(t, os.WriteFile(ktavPath, corrupt, 0o600))

	_, err := store.PersistMCPConfigResult(ScopeGlobal, "added", MCPConfig{
		Type: MCPHttp, URL: "http://added.example",
	})
	require.Error(t, err, "a corrupt Ktav document must abort the transaction")
	require.NotErrorIs(t, err, ErrMCPTargetExists, "corruption must not look like a duplicate-name refusal")
	require.NotErrorIs(t, err, ErrMCPNotFound, "corruption must not look like an absent document")
	require.NotContains(t, err.Error(), mcpKtavSecret, "the error must carry no document values")
	require.Equal(t, corrupt, fileBytes(t, ktavPath),
		"an aborted transaction must leave the bytes untouched")
}

// TestMCPKtavRevisionsTrackRawBytes pins that revisions of a Ktav document
// are driven by the RAW bytes: two documents that decode to identical JSON
// but differ in raw bytes advance the revision, while rewriting the exact
// same raw bytes does not.
func TestMCPKtavRevisionsTrackRawBytes(t *testing.T) {
	root, _, dataDir := mcpKtavIsolate(t)
	ktavPath := filepath.Join(dataDir, "rush.ktav")
	// Same semantics, different member order => different raw bytes after
	// ConvertJSONToKtav.
	first := mcpKtavFixture(`{"srv":{"type":"http","url":"http://same.example"}}`, mcpKtavSecret)
	second := `{"mcp":{"srv":{"type":"http","url":"http://same.example"}},"future_unknown_field":{"nested":[1,2,"three"]},"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"` + mcpKtavSecret + `","models":[{"id":"model","name":"Model"}]}}}`
	require.NotEqual(t, first, second)
	require.JSONEq(t, first, second)
	firstRaw := testKtavBytes(t, first)
	secondRaw := testKtavBytes(t, second)
	require.NotEqual(t, firstRaw, secondRaw, "the two encodings must differ in raw bytes")
	require.NoError(t, os.WriteFile(ktavPath, firstRaw, 0o600))

	store, err := Init(root, root, false)
	require.NoError(t, err)
	baseline := store.SnapshotMCPAdmission("srv").MCPRevision

	// Rewrite the SAME raw bytes: no revision movement.
	require.NoError(t, os.WriteFile(ktavPath, firstRaw, 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.Equal(t, baseline, store.SnapshotMCPAdmission("srv").MCPRevision,
		"an identical raw rewrite must not advance the revision")

	// Rewrite with an equivalent-but-differently-encoded document: the raw
	// fingerprint changes, so the revision must advance even though the
	// decoded configuration is unchanged.
	require.NoError(t, os.WriteFile(ktavPath, secondRaw, 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	require.Greater(t, store.SnapshotMCPAdmission("srv").MCPRevision, baseline,
		"a raw-bytes change must advance the revision even when the decode is identical")
	cfg, exists := store.MCPConfig("srv")
	require.True(t, exists)
	require.Equal(t, "http://same.example", cfg.URL)
}

// TestMCPKtavLockAnchorMatchesLegacySidecar pins that a rush.ktav write
// target anchors its inter-process sidecar on the LEGACY rush.json spelling:
// the lock file that appears on disk is rush.json.lock, so rush.ktav and
// rush.json stores contend on one sidecar.
func TestMCPKtavLockAnchorMatchesLegacySidecar(t *testing.T) {
	_, _, dataDir := mcpKtavIsolate(t)
	dir, err := filepath.EvalSymlinks(dataDir)
	require.NoError(t, err)
	ktavPath := filepath.Join(dir, "rush.ktav")
	legacyPath := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(ktavPath, testKtavBytes(t, `{"providers":{"custom":{"base_url":"https://example.invalid/v1","api_key":"k","models":[{"id":"model","name":"Model"}]}}}`), 0o600))

	require.Equal(t, legacyPath, configWriteLockAnchor(ktavPath))

	store := newTestConfigStore(testStoreOpts{config: &Config{}, globalDataPath: ktavPath})
	target, err := store.resolveConfigWriteTarget(ktavPath)
	require.NoError(t, err)
	require.Equal(t, normalizeReloadPath(legacyPath)+".lock", target.lockPath,
		"the .ktav record must anchor on the legacy-JSON sidecar")

	// The anchor must be the pathname the locking machinery actually takes:
	// acquiring it must create rush.json.lock (and only that sidecar).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lock, lockErr := acquireConfigFileLock(ctx, target.lockPath)
	require.NoError(t, lockErr)
	require.FileExists(t, target.lockPath)
	require.Equal(t, normalizeReloadPath(legacyPath), normalizeReloadPath(filepath.TrimSuffix(target.lockPath, ".lock")))
	require.NoFileExists(t, ktavPath+".lock")
	require.NoError(t, lock.Release(), "the sidecar handle must be released before TempDir cleanup")
}
