package modelatoms

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
// rather than glm-5.3, whose graduated 3-value set is covered separately by
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
// states: low/high/max — one more than every other Z.AI atom's off/on).
// VERIFIED 2026-08-26: GLM-5.3 (and GLM-5.3-Flash) reject disabling
// reasoning entirely, so "off" is deliberately NOT in this vocabulary
// anymore (it used to be, before that correction). A wider Z.AI-documented
// value like "xhigh" is also deliberately NOT accepted: this fork's
// coordinator.go collapses it to the same "max" wire value as an explicit
// "max", so it isn't a meaningful, distinct level to expose. See the
// comment on zai53ReasoningLevels in models_atoms.go.
func TestValidateEffortForModel_GLM53AcceptsGraduatedLevel(t *testing.T) {
	for _, level := range []string{"low", "high", "max"} {
		assert.NoError(t, validateEffortForModel("zai", "glm-5.3", level), "level %q", level)
	}
	// "off" is no longer part of glm-5.3's vocabulary — the API rejects
	// disabling reasoning for this model. "on" is a glm4_7_flash-style
	// boolean value, never part of glm-5.3's vocabulary at all.
	for _, level := range []string{"off", "on"} {
		err := validateEffortForModel("zai", "glm-5.3", level)
		require.Error(t, err, "level %q", level)
		assert.Contains(t, err.Error(), "not a valid effort level")
	}

	// "xhigh" is part of Z.AI's own documented reasoning_effort enum but
	// collapses to "max" on the wire — no longer accepted as a distinct
	// glm5_3 level (deliberately stricter than before this task).
	err := validateEffortForModel("zai", "glm-5.3", "xhigh")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid effort level")
}

// TestValidateEffortForModel_EmptyEffortAlwaysOK confirms omitting an
// effort suffix entirely (the common case) is always valid, atom or not.
func TestValidateEffortForModel_EmptyEffortAlwaysOK(t *testing.T) {
	assert.NoError(t, validateEffortForModel("zai", "glm-5.3", ""))
	assert.NoError(t, validateEffortForModel("openai", "gpt-5", ""))
}
