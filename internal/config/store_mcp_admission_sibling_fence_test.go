// Regression test for the round-5 Ktav activation failure: slot-sibling
// expansion tracks the rush.ktav sibling of every active rush.json, and the
// tracked snapshot stores a non-zero ABSENT fingerprint for the sibling
// (exists=false, but discovery/parent-identity/alias fields populated). The
// mutation-result staleness fence compares every non-zero tracked snapshot
// fingerprint against the fresh MCP evaluation; the evaluation covers only
// the documents MCP reads, never an absent sibling. Before the fix the fence
// reported ErrMCPMutationStale for EVERY lifecycle mutation, which failed
// ~34 internal/agent/tools/mcp tests ("MCP mutation result is stale").
package config

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMCPMutationFenceIgnoresAbsentKtavSibling(t *testing.T) {
	// Legacy world: gate OFF, rush.json active, rush.ktav absent. NOT
	// parallel: the pin mutates the shared ktavAutoMigrate global.
	pinKtavAutoMigrateOffForLegacyTest(t)
	store, root := isolatedMCPConfigStore(t)
	globalPath := GlobalConfigData()
	writeMCPDefinition(t, globalPath, "preexisting")

	// The reload publishes a tracked snapshot that contains the absent
	// rush.ktav sibling (reloadStalenessState expands slot siblings) with a
	// non-zero absent fingerprint.
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	sibling := normalizeDiscoveryPath(ktavSiblingOf(globalPath))
	require.NoFileExists(t, sibling)
	snap := store.loadSnapshot()
	require.Contains(t, snap.trackedConfigPaths, sibling,
		"precondition: the absent Ktav sibling must be a tracked path")
	siblingSnapshot, ok := snap.snapshots[sibling]
	require.True(t, ok, "precondition: the absent Ktav sibling must have a snapshot")
	require.False(t, siblingSnapshot.Exists)
	require.NotEqual(t, reloadFileFingerprint{}, siblingSnapshot.fingerprint,
		"precondition: the absent fingerprint must be non-zero for the fence to consult it")

	result, err := store.PersistMCPConfigResult(ScopeGlobal, "sibling-fence",
		MCPConfig{Type: MCPHttp, URL: "http://sibling-fence.example"})
	require.NoError(t, err)

	// The exact fence the mcp package's publish boundary runs:
	// WithCurrentMCPMutation -> mcpMutationResultCurrentLocked.
	require.NoError(t, store.WithCurrentMCPMutation(result, func() error { return nil }))
	require.NoFileExists(t, filepath.Join(root, "global-data", "rush.ktav"),
		"the legacy-world mutation must never create a Ktav sibling")
}
