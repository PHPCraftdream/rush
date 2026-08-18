// Effort-suffix validation for `crush models use`: the raw
// "provider/model@effort" syntax through the real CLI path (seedZAIProvider
// keeps zai resolvable) plus direct unit tests of validateEffortForModel.
package cmd

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedZAIProvider overwrites the isolated crush.json at globalPath with an
// explicit zai provider that carries a literal (self-resolving, non-empty)
// api_key.
//
// Any test that exercises the raw "zai/glm-4.7-flash[@effort]" syntax needs
// this. That path resolves through a.ResolveModel -> findModels, which
// iterates the LIVE, post-configureProviders provider map — and a provider
// only survives there if its API key resolves non-empty (see
// configureProviders' zai case in internal/config/load.go). isolatedModelsEnv
// has no ZAI_API_KEY/ZHIPU_API_KEY, so without this the zai provider is
// dropped and "zai/glm-4.7-flash" resolves to nothing.
//
// These tests historically passed ONLY by accident: isolatedModelsEnv
// redirects CRUSH_GLOBAL_DATA/XDG_DATA_HOME but NOT GlobalConfig()
// (CRUSH_GLOBAL_CONFIG/XDG_CONFIG_HOME), so on a dev machine with a real
// ~/.config/crush/crush.json configuring zai with an api_key, that host
// config leaked in and kept the provider alive. On a from-scratch CI runner
// (no such file, no env keys) the provider was absent and these tests failed
// with `model "zai/glm-4.7-flash" not found`. Seeding the provider explicitly
// makes the raw-resolution path deterministic, independent of host config.
// The model list itself still comes from the embedded catwalk catalog.
func seedZAIProvider(t *testing.T, globalPath string) {
	t.Helper()
	require.NoError(t, os.WriteFile(globalPath,
		[]byte(`{"providers":{"zai":{"api_key":"test-zai-key"}}}`), 0o644))
}

// TestModelsUse_RawZAIEffort_ValidSucceeds covers the new validated raw
// "provider/model@effort" path for a Z.AI atom: a level that IS one of the
// real declared levels (off/on, for a boolean-thinking-only model like
// glm-4.7-flash) must still be accepted and written. seedZAIProvider makes
// "zai/glm-4.7-flash" resolvable deterministically regardless of host config
// (the raw path goes through a.ResolveModel against the live provider map).
// The effort-validation logic itself is exercised precisely, atom-by-atom, by
// the TestValidateEffortForModel_* unit tests below.
func TestModelsUse_RawZAIEffort_ValidSucceeds(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	seedZAIProvider(t, globalPath)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"zai/glm-4.7-flash@on", "glm5_turbo")
	require.NoError(t, runErr)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	content := string(data)
	assert.Contains(t, content, `"glm-4.7-flash"`)
	assert.Contains(t, content, `"on"`)
}

// TestModelsUse_RawZAIEffort_TypoNowFailsCleanly is the load-bearing
// before/after test for this task: before adding a real, validated
// ReasoningLevels array for Z.AI atoms, ANY string after "@" in the raw
// "provider/model@effort" syntax (splitModelEffort) was accepted silently —
// a typo like "hgih" would have been written to config as-is and either
// ignored or mismapped by the wire-level provider code, with no error at
// all. Confirm that gap is now closed: an unsupported/typo'd level for a
// model that resolves to a KNOWN atom (glm-4.7-flash -> glm4_7_flash, which
// declares {"off","on"}) must now be rejected with a clear error.
func TestModelsUse_RawZAIEffort_TypoNowFailsCleanly(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	// The model must resolve first for effort validation to be reached; seed
	// zai so this asserts the effort-typo path, not a spurious "not found".
	seedZAIProvider(t, globalPath)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"zai/glm-4.7-flash@hgih", "glm5_turbo")
	require.Error(t, runErr, "a typo'd effort level for a known atom must now be rejected, not silently accepted")
	assert.Contains(t, runErr.Error(), "hgih")
	assert.Contains(t, runErr.Error(), "not a valid effort level")
	assert.Contains(t, runErr.Error(), "off|on")
}

// TestModelsUse_RawZAIEffort_TypoRejectedForWorkerSlot proves the same
// validation applies uniformly to the --worker/--reviewer role slots added
// in task #68, not just the two positional smart/fast args.
func TestModelsUse_RawZAIEffort_TypoRejectedForWorkerSlot(t *testing.T) {
	globalPath := isolatedModelsEnv(t)
	// The model must resolve first for effort validation to be reached; seed
	// zai so this asserts the effort-typo path, not a spurious "not found".
	seedZAIProvider(t, globalPath)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_turbo", "glm5_turbo", "--worker", "zai/glm-4.7-flash@ultramega")
	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "worker:")
	assert.Contains(t, runErr.Error(), "not a valid effort level")
}

// TestValidateEffortForModel_NonAtomStillUnvalidated is a regression guard,
// exercised directly against validateEffortForModel (models_atoms.go) rather
// than the full `models use` CLI path (which additionally requires the
// model to resolve against a real provider catalog — orthogonal to effort
// validation and awkward to fake in this isolated-config test harness): a
// (provider, model) pair that ISN'T in the atom registry at all has no
// levels array to validate against, so any effort string must still be
// accepted. This task closes the validation gap only for known atoms, not
// for arbitrary models outside the registry.
func TestValidateEffortForModel_NonAtomStillUnvalidated(t *testing.T) {
	err := validateEffortForModel("openai", "gpt-5", "totally-not-a-real-level")
	assert.NoError(t, err, "models outside the atom registry remain unvalidated by design")
}

// TestValidateEffortForModel_KnownAtomRejectsTypo is the direct-unit-test
// counterpart to TestModelsUse_RawZAIEffort_TypoNowFailsCleanly above: same
// fact, asserted without going through the full CLI/config-write path.
// Uses glm-4.7-flash (boolean off/on levels — see zaiBooleanThinkingLevels)
// rather than glm-5.3, whose graduated 8-value set is covered separately by
// TestValidateEffortForModel_GLM53AcceptsGraduatedLevel below.
func TestValidateEffortForModel_KnownAtomRejectsTypo(t *testing.T) {
	err := validateEffortForModel("zai", "glm-4.7-flash", "hgih")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hgih")
	assert.Contains(t, err.Error(), "not a valid effort level")
	assert.Contains(t, err.Error(), "off|on")
}

// TestValidateEffortForModel_KnownAtomAcceptsRealLevel confirms every level
// in a Z.AI atom's real declared array validates successfully, for a
// boolean-thinking-only model (glm-4.7-flash: only off/on, per Z.AI's docs —
// no documented graduated reasoning_effort outside GLM-5.3).
func TestValidateEffortForModel_KnownAtomAcceptsRealLevel(t *testing.T) {
	for _, level := range []string{"off", "on"} {
		assert.NoError(t, validateEffortForModel("zai", "glm-4.7-flash", level), "level %q", level)
	}
}

// TestValidateEffortForModel_GLM53AcceptsGraduatedLevel confirms GLM-5.3
// specifically validates against its OWN, larger vocabulary (3 real wire
// states: off/high/max — one more than every other Z.AI atom's off/on).
// A wider Z.AI-documented value like "xhigh" is deliberately NOT accepted:
// this fork's coordinator.go collapses it to the same "max" wire value as
// an explicit "max", so it isn't a meaningful, distinct level to expose.
// See the comment on zaiReasoningLevels in models_atoms.go.
func TestValidateEffortForModel_GLM53AcceptsGraduatedLevel(t *testing.T) {
	for _, level := range []string{"off", "high", "max"} {
		assert.NoError(t, validateEffortForModel("zai", "glm-5.3", level), "level %q", level)
	}
	// "on" is a glm4_7_flash-style boolean value, not part of glm-5.3's
	// vocabulary — must still be rejected for glm-5.3.
	err := validateEffortForModel("zai", "glm-5.3", "on")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid effort level")

	// "xhigh" is part of Z.AI's own documented reasoning_effort enum but
	// collapses to "max" on the wire — no longer accepted as a distinct
	// glm5_3 level (deliberately stricter than before this task).
	err = validateEffortForModel("zai", "glm-5.3", "xhigh")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid effort level")
}

// TestValidateEffortForModel_EmptyEffortAlwaysOK confirms omitting an
// effort suffix entirely (the common case) is always valid, atom or not.
func TestValidateEffortForModel_EmptyEffortAlwaysOK(t *testing.T) {
	assert.NoError(t, validateEffortForModel("zai", "glm-5.3", ""))
	assert.NoError(t, validateEffortForModel("openai", "gpt-5", ""))
}
