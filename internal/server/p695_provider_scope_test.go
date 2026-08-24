package server

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// Task #695 (review finding F7) — the ConfigWire provider entries must
// expose the scope the provider's effective config is governed by, so the
// web edit form can prefill its scope selector instead of assuming global
// and writing a new global entry under a still-active local override.

// TestProviderWireIncludesScope locks the wire JSON shape for the new
// field, mirroring TestProviderWireIncludesPeakHours's struct-tag contract
// style.
func TestProviderWireIncludesScope(t *testing.T) {
	out, err := json.Marshal(ProviderWire{Name: "x", Enabled: true, Scope: "local"})
	require.NoError(t, err)
	require.Contains(t, string(out), `"scope":"local"`)

	out2, err := json.Marshal(ProviderWire{Name: "y", Enabled: false})
	require.NoError(t, err)
	require.NotContains(t, string(out2), `"scope"`, "zero value must stay omitted (omitempty)")
}

// TestBuildConfigWire_ReportsEffectiveProviderScope seeds a real provider
// via the actual WS handlers and asserts the wire flips global → local
// once a workspace override shadows the global entry.
func TestBuildConfigWire_ReportsEffectiveProviderScope(t *testing.T) {
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	store := a.Store()
	require.True(t, store.HasWorkspaceConfig(), "test app must resolve a workspace config path")

	addPayload, err := json.Marshal(AddCustomProviderPayload{
		ID:      "acme",
		BaseURL: "https://acme.example.invalid/v1",
		Type:    "openai-compat",
		Scope:   "global",
		Models:  []CustomModelPayload{{ID: "acme-m", Name: "Acme M"}},
	})
	require.NoError(t, err)
	handleAddCustomProvider(a, newTestClient(), WSMessage{ID: "seed-add", Type: CmdAddCustomProvider, Payload: addPayload})

	wire, ok := buildConfigWire(a)
	require.True(t, ok)
	require.Equal(t, "global", wire.Providers["acme"].Scope, "no workspace override yet — effective scope must be global")

	// Update targeting local scope: writes providers.acme into the
	// workspace config while the global entry from the add stays on disk —
	// the workspace entry must now win the merge.
	updatePayload, err := json.Marshal(UpdateCustomProviderPayload{
		OldID:   "acme",
		ID:      "acme",
		BaseURL: "https://acme.example.invalid/v1",
		Type:    "openai-compat",
		Scope:   "local",
		Models:  []CustomModelPayload{{ID: "acme-m", Name: "Acme M"}},
	})
	require.NoError(t, err)
	handleUpdateCustomProvider(a, newTestClient(), WSMessage{ID: "seed-update", Type: CmdUpdateCustomProvider, Payload: updatePayload})

	wire2, ok := buildConfigWire(a)
	require.True(t, ok)
	require.Equal(t, "local", wire2.Providers["acme"].Scope, "workspace override must win and report local")

	// Ground truth on disk: BOTH files hold the entry; only the workspace
	// one makes the effective scope local.
	require.True(t, store.HasConfigField(config.ScopeWorkspace, "providers.acme"))
	require.True(t, store.HasConfigField(config.ScopeGlobal, "providers.acme"))
}
