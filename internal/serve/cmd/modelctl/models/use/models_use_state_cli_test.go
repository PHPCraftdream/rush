// `rush models state` tests: worker/reviewer reporting in text and --json
// modes, unset-effort default notes, and unit tests of the note helpers
// (unsetEffortNote, effortEffectiveNote, nilOrEffortDefault).
package modeluse

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelsState_ReportsWorkerAndReviewer(t *testing.T) {
	ktavGateOn(t)
	setupOwnerCommands(t)

	resetModelsUseFlags(t, ownerUseCmd)
	_, runErr := runModelsCmd(t, ownerUseCmd,
		"glm5_turbo", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.NoError(t, runErr)

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd)
	require.NoError(t, runErr)

	workerRow, ok := effectiveRow(out, "worker")
	require.True(t, ok, "expected a worker row in:\n%s", out)
	assert.Contains(t, workerRow, "glm-4.7-flash")
	reviewerRow, ok := effectiveRow(out, "reviewer")
	require.True(t, ok, "expected a reviewer row in:\n%s", out)
	assert.Contains(t, reviewerRow, "glm-5.3")
}

func TestModelsState_JSONReportsWorkerAndReviewer(t *testing.T) {
	ktavGateOn(t)
	setupOwnerCommands(t)

	resetModelsUseFlags(t, ownerUseCmd)
	_, runErr := runModelsCmd(t, ownerUseCmd,
		"glm5_turbo", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.NoError(t, runErr)

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd, "--json")
	require.NoError(t, runErr)

	assert.Contains(t, out, `"worker"`)
	assert.Contains(t, out, `"reviewer"`)
	assert.Contains(t, out, "glm-4.7-flash")
	assert.Contains(t, out, "glm-5.3")
}

// TestModelsState_UnsetEffort_ShowsKnownZAIDefault covers the core case for
// this task: a slot with no explicit effort on a provider with a KNOWN
// documented unset-default (Z.AI, "unset -> thinking on, high" per
// coordinator_providers.go's getProviderOptions and providerEffortDocs in
// models_efforts.go) must show that default as a terse parenthetical.
// "glm5_turbo" (large, in this test) is set with no @effort suffix, so
// m.ReasoningEffort == "" and effortEffectiveNote must fire.
func TestModelsState_UnsetEffort_ShowsKnownZAIDefault(t *testing.T) {
	ktavGateOn(t)
	setupOwnerCommands(t)

	resetModelsUseFlags(t, ownerUseCmd)
	_, runErr := runModelsCmd(t, ownerUseCmd, "glm5_turbo", "glm5_turbo")
	require.NoError(t, runErr)

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd)
	require.NoError(t, runErr)

	assert.Contains(t, out, "unset -> thinking on, high")
}

// TestModelsState_ExplicitEffort_TakesPrecedenceOverDefault covers the
// no-confusion requirement: a slot WITH an explicit effort must show that
// explicit value (via the existing effortSuffix " effort=<level>" rendering),
// not the provider's unset-default note — the two must never appear together
// on the same slot's line, or a reader can't tell "you set this" from "this
// merely happens by default". Uses glm4_7_flash (boolean off/on Z.AI atom) rather than
// glm5_3: the embedded catwalk provider catalog this test env falls back to
// in RUSH_PROVIDER_CACHE_ONLY mode doesn't list glm-5.3 at all, which makes
// config's smart/fast validation silently substitute a different zai model
// on load — an unrelated, pre-existing environmental quirk of the vendored
// catwalk embedded data, not something this task touches. glm4_7_flash IS in
// that embedded list, so it round-trips through config load untouched.
func TestModelsState_ExplicitEffort_TakesPrecedenceOverDefault(t *testing.T) {
	ktavGateOn(t)
	setupOwnerCommands(t)

	resetModelsUseFlags(t, ownerUseCmd)
	_, runErr := runModelsCmd(t, ownerUseCmd, "glm4_7_flash-on", "glm5_turbo")
	require.NoError(t, runErr)

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd)
	require.NoError(t, runErr)

	smartLine, ok := effectiveRow(out, "smart")
	require.True(t, ok, "expected a smart row in output:\n%s", out)
	assert.Equal(t, "on", tableCells(smartLine)[3], "the EFFORT column holds the explicit effort")
	assert.NotContains(t, smartLine, "unset ->")
}

// tableCells splits a bordered table row into its trimmed cells.
func tableCells(line string) []string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(line), "│"), "│")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// effectiveRow returns the EFFECTIVE table row for role: the first line
// whose first cell is role.
func effectiveRow(s, role string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		if cells := tableCells(line); len(cells) > 0 && cells[0] == role {
			return line, true
		}
	}
	return "", false
}

// TestModelsState_JSON_UnsetEffort_ShowsKnownDefault verifies the --json
// mode also surfaces the same fact machine-readably (nilOrEffortDefault),
// not just the human text rendering.
func TestModelsState_JSON_UnsetEffort_ShowsKnownDefault(t *testing.T) {
	ktavGateOn(t)
	setupOwnerCommands(t)

	resetModelsUseFlags(t, ownerUseCmd)
	_, runErr := runModelsCmd(t, ownerUseCmd, "glm5_turbo", "glm5_turbo")
	require.NoError(t, runErr)

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd, "--json")
	require.NoError(t, runErr)

	var doc struct {
		Effective struct {
			SmartEffortDefault *string `json:"smart_effort_default"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	require.NotNil(t, doc.Effective.SmartEffortDefault)
	assert.Equal(t, "unset -> thinking on, high", *doc.Effective.SmartEffortDefault)
}

// TestModelsState_JSON_ExplicitEffort_NullDefault verifies the JSON default
// field is null (not the fact string) when the slot has an explicit effort —
// the machine-readable mirror of the text-mode precedence test above. See
// the comment on TestModelsState_ExplicitEffort_TakesPrecedenceOverDefault
// for why glm4_7_flash is used instead of glm5_3 here.
func TestModelsState_JSON_ExplicitEffort_NullDefault(t *testing.T) {
	ktavGateOn(t)
	setupOwnerCommands(t)

	resetModelsUseFlags(t, ownerUseCmd)
	_, runErr := runModelsCmd(t, ownerUseCmd, "glm4_7_flash-on", "glm5_turbo")
	require.NoError(t, runErr)

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd, "--json")
	require.NoError(t, runErr)

	var doc struct {
		Effective struct {
			SmartEffortDefault *string `json:"smart_effort_default"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.Nil(t, doc.Effective.SmartEffortDefault)
}

// TestModelsState_EndToEnd_FlagsStaleGLM53FlashEffort is the end-to-end
// regression test for the exact scenario an operator hit live: a global
// config written before the 2026-08-26 GLM-5.3-Flash correction (when the
// atom still declared boolean off/on) persisted `reasoning_effort: "on"`.
// `rush models state` used to render that as `(atom: glm5_3_flash-on)` —
// implying `rush models use glm5_3_flash-on` would still work, when the
// atom's real vocabulary is now low/high/max and that exact command is
// rejected. This test seeds that stale value directly on disk (it can no
// longer be produced via `rush models use`, since the atom now validates
// against low/high/max) and asserts on the REAL rendered CLI output, not
// just the note helpers in isolation.
func TestModelsState_EndToEnd_FlagsStaleGLM53FlashEffort(t *testing.T) {
	ktavGateOn(t)
	globalPath := setupOwnerCommands(t)

	require.NoError(t, os.WriteFile(globalPath, []byte(`{
		"providers": {"zai": {"api_key": "test-zai-key"}},
		"models": {
			"smart": {"provider": "zai", "model": "glm-5.3-flash", "reasoning_effort": "on"},
			"fast": {"provider": "zai", "model": "glm-5-turbo"}
		}
	}`), 0o644))

	resetOwnerStateFlags(t, ownerStateCmd)
	out, runErr := runModelsCmd(t, ownerStateCmd)
	require.NoError(t, runErr)

	smartLine, ok := effectiveRow(out, "smart")
	require.True(t, ok, "expected a smart row in output:\n%s", out)
	assert.Contains(t, smartLine, "STALE:")
	assert.Contains(t, smartLine, `"on"`)
	assert.Contains(t, smartLine, "low|high|max")
	assert.NotContains(t, smartLine, "glm5_3_flash-on",
		"must never render a <atom>-<effort> form that `rush models use` would now reject")

	// --json must carry the same fact machine-readably.
	resetOwnerStateFlags(t, ownerStateCmd)
	jsonOut, runErr := runModelsCmd(t, ownerStateCmd, "--json")
	require.NoError(t, runErr)
	assert.Contains(t, jsonOut, `"smart_effort_stale":["low","high","max"]`)
}
