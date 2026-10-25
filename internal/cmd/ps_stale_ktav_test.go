// Tests for readPsSlots against Ktav-format global config slots.
//
// Revert-checks (one single-line production change each; the orchestrator
// runs the mutants, never this file):
//
//	TestReadPsSlotsKtavSlotSelected: make readPsSlots read the legacy JSON
//	  spelling directly (os.ReadFile(config.GlobalConfigData())) instead of
//	  config.ReadConfigDocumentLight — the Ktav-only and both-present cases
//	  would report known=false or the stale JSON values.
//	TestReadPsSlotsBackupOnlyUnknown: make readPsSlots ignore the
//	  ReadConfigDocumentLight error (e.g. treat non-NotExist errors as an
//	  empty document) — the backup-only case would report known=true with
//	  no slots instead of degrading to unknown.
//	TestReadPsSlotsJSONUnchanged: make readPsSlots drop the
//	  `m.Model != ""` guard (or the slot-name filter) — the empty-model
//	  slot would appear in bySlot and break the pinned output.
package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
)

// TestReadPsSlotsKtavSlotSelected seeds only a rush.ktav next to the
// isolated GlobalConfigData location and asserts the slots are read from
// it (the Ktav sibling is selected and decoded).
func TestReadPsSlotsKtavSlotSelected(t *testing.T) {
	// NOT parallel: psIsolateEnv uses t.Setenv, which forbids t.Parallel.
	_, dataDir := psIsolateEnv(t)
	jsonDoc := []byte(`{"models":{"smart":{"provider":"p1","model":"m1"},"fast":{"provider":"p2","model":"m2"}}}`)
	ktavDoc, err := ktavcodec.ConvertJSONToKtav(jsonDoc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.ktav"), ktavDoc, 0o644))

	slots := readPsSlots()
	require.True(t, slots.known)
	require.Equal(t, psSlotModel{Provider: "p1", Model: "m1"}, slots.bySlot["smart"])
	require.Equal(t, psSlotModel{Provider: "p2", Model: "m2"}, slots.bySlot["fast"])
}

// TestReadPsSlotsBothPresentKtavWins seeds both spellings with different
// values and asserts the Ktav document wins.
func TestReadPsSlotsBothPresentKtavWins(t *testing.T) {
	_, dataDir := psIsolateEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"),
		[]byte(`{"models":{"smart":{"provider":"json-p","model":"json-m"}}}`), 0o644))
	ktavDoc, err := ktavcodec.ConvertJSONToKtav([]byte(`{"models":{"smart":{"provider":"ktav-p","model":"ktav-m"}}}`))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.ktav"), ktavDoc, 0o644))

	slots := readPsSlots()
	require.True(t, slots.known)
	require.Equal(t, psSlotModel{Provider: "ktav-p", Model: "ktav-m"}, slots.bySlot["smart"])
}

// TestReadPsSlotsBackupOnlyUnknown seeds only rush.json.bak next to the
// isolated GlobalConfigData location: the light reader fails closed and
// readPsSlots must degrade to known=false.
func TestReadPsSlotsBackupOnlyUnknown(t *testing.T) {
	_, dataDir := psIsolateEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json.bak"),
		[]byte(`{"models":{"smart":{"provider":"p","model":"m"}}}`), 0o644))

	slots := readPsSlots()
	require.False(t, slots.known)
	require.Empty(t, slots.bySlot)
}

// TestReadPsSlotsJSONUnchanged pins the pre-Ktav behavior for a plain
// rush.json, including the empty-model guard.
func TestReadPsSlotsJSONUnchanged(t *testing.T) {
	_, dataDir := psIsolateEnv(t)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"),
		[]byte(`{"models":{"smart":{"provider":"p","model":"m"},"fast":{"provider":"q","model":""}}}`), 0o644))

	slots := readPsSlots()
	require.True(t, slots.known)
	require.Equal(t, map[string]psSlotModel{"smart": {Provider: "p", Model: "m"}}, slots.bySlot)
}
