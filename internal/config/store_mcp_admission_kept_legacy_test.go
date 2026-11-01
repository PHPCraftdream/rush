// Regression tests for the Ktav activation state every migrated user is in:
// migration publishes rush.ktav and KEEPS the legacy rush.json beside it
// (ktavKeepsLegacy). Slot-sibling expansion tracks both spellings, but the MCP
// evaluation reads only the selected rush.ktav. The mutation-result fence used
// to treat the EXISTING, unread rush.json as stale, so every lifecycle
// mutation (enable, disable, add, replace) failed with "MCP mutation result is
// stale". It must be stale only when the sibling's real bytes change.
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// keptLegacySiblingStore returns a store whose global data slot was migrated
// to Ktav with the legacy JSON kept beside it. NOT parallel: the migration
// gate is a shared global.
func keptLegacySiblingStore(t *testing.T) (*ConfigStore, string) {
	t.Helper()
	restore := SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)
	root, _, dataDir := mcpKtavIsolate(t)
	legacyPath := filepath.Join(dataDir, "rush.json")
	ktavPath := filepath.Join(dataDir, "rush.ktav")
	require.NoError(t, os.WriteFile(legacyPath,
		[]byte(mcpKtavFixture(`{"existing":{"type":"http","url":"http://existing.example"}}`, "k")), 0o600))

	store, err := Init(root, root, false)
	require.NoError(t, err)
	require.FileExists(t, ktavPath, "precondition: migration publishes the Ktav generation")
	require.FileExists(t, legacyPath, "precondition: migration keeps the legacy JSON beside it")
	tracked := normalizeDiscoveryPath(legacyPath)
	snap := store.loadSnapshot()
	require.Contains(t, snap.trackedConfigPaths, tracked,
		"precondition: the kept legacy sibling must be a tracked path")
	require.True(t, snap.snapshots[tracked].Exists,
		"precondition: the kept legacy sibling must be recorded as existing")
	return store, legacyPath
}

func TestMCPMutationFenceIgnoresUnchangedKeptLegacySibling(t *testing.T) {
	store, _ := keptLegacySiblingStore(t)

	result, err := store.PersistMCPConfigResult(ScopeGlobal, "kept-legacy",
		MCPConfig{Type: MCPHttp, URL: "http://kept-legacy.example"})
	require.NoError(t, err)

	require.NoError(t, store.WithCurrentMCPMutation(result, func() error { return nil }),
		"an existing but unchanged and unread legacy sibling must not make the mutation stale")
}

func TestMCPMutationFenceDetectsChangedKeptLegacySibling(t *testing.T) {
	store, legacyPath := keptLegacySiblingStore(t)

	result, err := store.PersistMCPConfigResult(ScopeGlobal, "kept-legacy",
		MCPConfig{Type: MCPHttp, URL: "http://kept-legacy.example"})
	require.NoError(t, err)

	// A foreign write to the kept sibling after the mutation is a real change
	// of a tracked input: the fence must still report it.
	data, err := os.ReadFile(legacyPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(legacyPath, append(data, '\n'), 0o600))

	require.ErrorIs(t, store.WithCurrentMCPMutation(result, func() error { return nil }), ErrMCPMutationStale)
}
