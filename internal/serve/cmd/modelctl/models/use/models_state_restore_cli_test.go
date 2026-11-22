package modeluse

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelsState_UnsetRoleScopeLabels(t *testing.T) {
	ktavGateOn(t)
	setupOwnerCommands(t)

	resetOwnerStateFlags(t, ownerStateCmd)
	text, runErr := runModelsCmd(t, ownerStateCmd)
	require.NoError(t, runErr)
	smartRow, ok := effectiveRow(text, "smart")
	require.True(t, ok, "expected a smart row in output:\n%s", text)
	assert.Equal(t, "default", tableCells(smartRow)[5], "SCOPE column follows EFFORT and TIER")

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd, "--json")
	require.NoError(t, runErr)
	var doc struct {
		Effective struct {
			SmartScope string `json:"smart_scope"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.Empty(t, doc.Effective.SmartScope, "JSON scope stays empty when neither scope sets the role")
}

func TestRestoreCommands_EmptyState(t *testing.T) {
	ktavGateOn(t)
	setupOwnerCommands(t)

	resetOwnerStateFlags(t, ownerStateCmd)
	text, runErr := runModelsCmd(t, ownerStateCmd)
	require.NoError(t, runErr)
	assert.Contains(t, text, "RESTORE (run these to re-create this state)")
	assert.Contains(t, text, "(nothing is set explicitly)")

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd, "--json")
	require.NoError(t, runErr)
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(out), &top))
	require.Contains(t, top, "restore_commands")
	require.Equal(t, "[]", string(top["restore_commands"]), "empty restore_commands must marshal as [] not null")
}

func TestRestoreCommands_NoSecretLeakage(t *testing.T) {
	ktavGateOn(t)
	globalPath := setupOwnerCommands(t)

	fixture := `{"providers":{"zai":{"api_key":"sk-FAKE-SECRET-abc123"}},` +
		`"models":{"smart":{"provider":"zai","model":"glm-4.6","reasoning_effort":"on"}}}`
	require.NoError(t, os.WriteFile(globalPath, []byte(fixture), 0o644))
	onDisk, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.Contains(t, string(onDisk), "sk-FAKE-SECRET-abc123")

	resetOwnerStateFlags(t, ownerStateCmd)
	text, runErr := runModelsCmd(t, ownerStateCmd)
	require.NoError(t, runErr)
	assert.NotContains(t, text, "sk-FAKE-SECRET-abc123")
	assert.Contains(t, text, "rush models use --smart glm4_6-on")

	resetOwnerStateFlags(t, ownerStateCmd)
	jsonOut, runErr := runModelsCmd(t, ownerStateCmd, "--json")
	require.NoError(t, runErr)
	assert.NotContains(t, jsonOut, "sk-FAKE-SECRET-abc123")
}
