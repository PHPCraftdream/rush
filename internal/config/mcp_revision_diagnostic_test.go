package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Both cases exercise the shared MCP revision/admission paths with each
// global settings representation. Do not parallelize: ktavAutoMigrate is global.
func TestMCPRevisionDiagnosticsAcrossGlobalStorePaths(t *testing.T) {
	for _, migration := range []struct {
		name    string
		enabled bool
	}{
		{name: "legacy-json", enabled: false},
		{name: "ktav", enabled: true},
	} {
		t.Run(migration.name, func(t *testing.T) {
			restore := SetKtavAutoMigrateForTest(migration.enabled)
			t.Cleanup(restore)

			t.Run("other-store-mutation-does-not-invalidate-admission", func(t *testing.T) {
				store, _ := isolatedMCPConfigStore(t)
				require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "A", MCPConfig{Type: MCPHttp, URL: "http://a.example"}))
				require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "B", MCPConfig{Type: MCPHttp, URL: "http://b.example"}))
				require.NoError(t, store.PersistMCPFieldsExact(ScopeGlobal, "A", map[string]any{"url": "http://a-updated.example"}))

				snapshot := store.SnapshotMCPAdmission("B")
				require.NoError(t, store.WithCurrentMCPAdmissionContext(context.Background(), snapshot, "B", func(MCPAdmissionGuard) error {
					return nil
				}))
			})

			t.Run("global-data-mutation-does-not-invalidate-B-admission", func(t *testing.T) {
				root, _, _ := mcpKtavIsolate(t)
				configPath := GlobalConfig()
				dataPath := GlobalConfigData()
				if migration.enabled {
					configPath = ktavSiblingOf(configPath)
					dataPath = ktavSiblingOf(dataPath)
				}
				seed := []byte(`{"mcp":{"B":{"type":"http","url":"http://b.example"}}}`)
				if migration.enabled {
					seed = testKtavBytes(t, string(seed))
				}
				require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o700))
				require.NoError(t, os.WriteFile(configPath, seed, 0o600))
				// Ensure the app-managed data document exists before Init; it has no B.
				dataDoc := []byte(`{"recent_models":{"smart":[]}}`)
				if migration.enabled {
					dataDoc = testKtavBytes(t, string(dataDoc))
				}
				require.NoError(t, os.MkdirAll(filepath.Dir(dataPath), 0o700))
				require.NoError(t, os.WriteFile(dataPath, dataDoc, 0o600))

				store, err := Init(root, root, false)
				require.NoError(t, err)
				require.Equal(t, configPath, ActiveConfigFileName(GlobalConfig()))
				require.Equal(t, dataPath, ActiveConfigFileName(GlobalConfigData()))
				assertBStable := func(before MCPAdmissionSnapshot) {
					after := store.SnapshotMCPAdmission("B")
					require.Equal(t, before.MCPRevision, after.MCPRevision)
					require.Equal(t, before.MCPInput, after.MCPInput)
					require.NoError(t, store.WithCurrentMCPAdmissionContext(context.Background(), before, "B", func(MCPAdmissionGuard) error { return nil }))
				}
				before := store.SnapshotMCPAdmission("B")
				_, exists := store.MCPConfig("B")
				require.True(t, exists)
				dataBytes, err := os.ReadFile(dataPath)
				require.NoError(t, err)
				initialDataBytes := append([]byte(nil), dataBytes...)
				require.NotContains(t, string(dataBytes), `"B"`)

				require.NoError(t, store.SetConfigField(ScopeGlobal, "recent_models.smart", []SelectedModel{{Provider: "p", Model: "different"}}))
				require.NoError(t, store.ReloadFromDisk(context.Background()))
				assertBStable(before)

				// The data document changed while B stayed absent from decoded data.
				dataBytes, err = os.ReadFile(dataPath)
				require.NoError(t, err)
				require.NotEqual(t, initialDataBytes, dataBytes)
				raw := dataBytes
				if migration.enabled {
					raw = mustDecodeTestKtav(t, dataBytes)
				}
				require.NotContains(t, string(raw), `"B"`)

				// Rewrite an unrelated key in the data document's actual disk format.
				dataBytes, err = os.ReadFile(dataPath)
				require.NoError(t, err)
				raw = dataBytes
				if migration.enabled {
					raw = mustDecodeTestKtav(t, dataBytes)
				}
				var document map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(raw, &document))
				document["unrelated_diagnostic_key"] = json.RawMessage(`true`)
				rewritten, err := json.Marshal(document)
				require.NoError(t, err)
				if migration.enabled {
					rewritten = testKtavBytes(t, string(rewritten))
				}
				require.NoError(t, os.WriteFile(dataPath, rewritten, 0o600))
				require.NoError(t, store.ReloadFromDisk(context.Background()))
				assertBStable(before)
			})

			// A Ktav revision deliberately tracks the RAW bytes of the document
			// that defines the entry (TestMCPKtavRevisionsTrackRawBytes), so an
			// unrelated field written into that SAME document legitimately
			// advances it; the cross-document case above is the Ktav contract.
			if !migration.enabled {
				t.Run("unrelated-data-reload-preserves-revision", func(t *testing.T) {
					store, _ := isolatedMCPConfigStore(t)
					require.NoError(t, store.PersistMCPConfig(ScopeGlobal, "stable", MCPConfig{Type: MCPHttp, URL: "http://stable.example"}))
					before := store.SnapshotMCPAdmission("stable")
					require.NoError(t, store.SetConfigField(ScopeGlobal, "recent_models.smart", []any{}))
					require.NoError(t, store.ReloadFromDisk(context.Background()))
					after := store.SnapshotMCPAdmission("stable")
					require.Equal(t, before.MCPRevision, after.MCPRevision,
						"unrelated global data settings must not advance an MCP revision")
					require.Equal(t, before.MCPInput, after.MCPInput)
				})
			}
		})
	}
}
