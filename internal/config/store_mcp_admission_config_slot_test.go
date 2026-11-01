// Regression tests for the second half of the Ktav activation state: the
// GLOBAL CONFIG slot (providers, MCP servers) is migrated like the data slot,
// and the legacy rush.json stays beside rush.ktav. The loader reads only the
// selected rush.ktav, but the MCP evaluation used to add the fixed legacy
// spelling as a second document, so its input fingerprint of every server
// differed from the loaded snapshot's and every admission (initialize, enable,
// disable) reported "MCP mutation result is stale".
package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// keptLegacyConfigSlotStore returns a store whose global config slot AND data
// slot were migrated to Ktav with the legacy JSON kept beside each. NOT
// parallel: the migration gate is a shared global.
func keptLegacyConfigSlotStore(t *testing.T) *ConfigStore {
	t.Helper()
	restore := SetKtavAutoMigrateForTest(true)
	t.Cleanup(restore)
	root, configDir, dataDir := mcpKtavIsolate(t)
	configLegacy := filepath.Join(configDir, "rush.json")
	require.NoError(t, os.WriteFile(configLegacy,
		[]byte(mcpKtavFixture(`{"from-config":{"type":"http","url":"http://from-config.example"}}`, "k")), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"),
		[]byte(`{"recent_models":{"smart":[]}}`), 0o600))

	store, err := Init(root, root, false)
	require.NoError(t, err)
	require.FileExists(t, filepath.Join(configDir, "rush.ktav"), "precondition: the config slot is migrated")
	require.FileExists(t, configLegacy, "precondition: migration keeps the legacy config JSON beside it")
	return store
}

func TestMCPAdmissionAcceptsServersWithMigratedConfigSlot(t *testing.T) {
	store := keptLegacyConfigSlotStore(t)
	require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "added",
		MCPConfig{Type: MCPHttp, URL: "http://added.example"}))

	for _, name := range []string{"from-config", "added"} {
		t.Run(name, func(t *testing.T) {
			snapshot := store.SnapshotMCPAdmission(name)
			require.True(t, snapshot.Exists, "precondition: the server is in the loaded snapshot")
			require.True(t, snapshot.HasMCPInput)
			require.NoError(t, store.WithCurrentMCPAdmissionContext(context.Background(), snapshot, name,
				func(MCPAdmissionGuard) error { return nil }),
				"the evaluation must fingerprint the same documents as the loader, so an untouched server is current")
		})
	}
}

func TestMCPMutationAcceptsResultWithMigratedConfigSlot(t *testing.T) {
	store := keptLegacyConfigSlotStore(t)

	result, err := store.PersistMCPConfigResult(ScopeGlobal, "added",
		MCPConfig{Type: MCPHttp, URL: "http://added.example"})
	require.NoError(t, err)
	require.NoError(t, store.WithCurrentMCPMutation(result, func() error { return nil }))
}
