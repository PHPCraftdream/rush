package modeluse

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	modelatoms "github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models/atoms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnsetEffortNote_KnownAndUnknownProviders is the direct-unit-test
// counterpart of the CLI-level tests above (same rationale as
// TestValidateEffortForModel_NonAtomStillUnvalidated: exercising provider
// resolution through the full `models use`/`models state` CLI path for a
// raw, non-atom provider/model requires the model to resolve against a real
// provider catalog, which this isolated-config test harness can't reliably
// fake). Covers all three requirements from this task in one place: a
// KNOWN default for Z.AI, the OPPOSITE known default for DeepSeek (proving
// unsetEffortNote actually branches per provider instead of hardcoding one
// fact), and silence ("") for io.net/hyper/Alibaba/generic-openai, none of
// which has a single unambiguous unset-effort fact documented in
// providerEffortDocs — effortEffectiveNote and nilOrEffortDefault must never
// guess for those.
func TestUnsetEffortNote_KnownAndUnknownProviders(t *testing.T) {
	assert.Equal(t, "unset -> thinking on, high", modelatoms.UnsetEffortNote("zai"))
	assert.Equal(t, "unset -> thinking off", modelatoms.UnsetEffortNote("deepseek"))

	for _, provider := range []string{"ionet", "hyper", "alibaba-singapore", "openai", "anthropic", "local-cli"} {
		assert.Empty(t, modelatoms.UnsetEffortNote(provider), "provider %q has no documented unset-default fact and must not guess one", provider)
	}
}

// TestEffortEffectiveNote_ExplicitVsUnset is a direct unit test of the
// precedence rule effortEffectiveNote implements: an explicit effort always
// wins (nothing extra to print — effortSuffix already shows the explicit
// value), and an unset effort shows the provider's known default, or nothing
// for an undocumented provider.
func TestEffortEffectiveNote_ExplicitVsUnset(t *testing.T) {
	// Explicit effort set: no default note, regardless of provider.
	explicit := config.SelectedModel{Provider: "zai", Model: "glm-5.3", ReasoningEffort: "max"}
	assert.Empty(t, effortEffectiveNote(explicit))

	// Unset effort, known Z.AI default: note must appear.
	unsetZAI := config.SelectedModel{Provider: "zai", Model: "glm-5.3"}
	assert.Contains(t, effortEffectiveNote(unsetZAI), "unset -> thinking on, high")

	// Unset effort, undocumented provider: must stay silent, not guess.
	unsetUnknown := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	assert.Empty(t, effortEffectiveNote(unsetUnknown))
}

// TestNilOrEffortDefault_JSONCounterpart mirrors the text-mode test above
// for the --json path's nilOrEffortDefault helper.
func TestNilOrEffortDefault_JSONCounterpart(t *testing.T) {
	explicit := config.SelectedModel{Provider: "zai", Model: "glm-5.3", ReasoningEffort: "max"}
	assert.Nil(t, nilOrEffortDefault(true, explicit))

	unsetZAI := config.SelectedModel{Provider: "zai", Model: "glm-5.3"}
	assert.Equal(t, "unset -> thinking on, high", nilOrEffortDefault(true, unsetZAI))

	unsetUnknown := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	assert.Nil(t, nilOrEffortDefault(true, unsetUnknown))

	// Slot not set at all: nil regardless of provider/effort.
	assert.Nil(t, nilOrEffortDefault(false, unsetZAI))
}

// TestStaleEffortNote_FlagsVocabularyChange covers the exact regression the
// 2026-08-26 GLM-5.3 correction created: a config written while
// glm5_3_flash still declared the boolean off/on vocabulary keeps that value
// on disk, but "on" is no longer one of the atom's levels. `models state`
// must say so rather than rendering "glm5_3_flash-on" as if that were a form
// `rush models use` would still accept.
func TestStaleEffortNote_FlagsVocabularyChange(t *testing.T) {
	stale := config.SelectedModel{Provider: "zai", Model: "glm-5.3-flash", ReasoningEffort: "on"}
	note := staleEffortNote(stale)
	require.NotEmpty(t, note, "an effort outside the atom's levels must be flagged")
	assert.Contains(t, note, "STALE")
	assert.Contains(t, note, `"on"`)
	assert.Contains(t, note, "low|high|max", "the note must name the levels that ARE valid")

	// effortEffectiveNote must surface the same fact on the rendered line —
	// asserted against the expected content directly (not against
	// staleEffortNote's own return value, which would be tautological given
	// effortEffectiveNote's current one-line delegation).
	rendered := effortEffectiveNote(stale)
	assert.Contains(t, rendered, "STALE")
	assert.Contains(t, rendered, `"on"`)
	assert.Contains(t, rendered, "low|high|max")

	// A valid effort on the same atom stays silent.
	for _, level := range []string{"low", "high", "max"} {
		ok := config.SelectedModel{Provider: "zai", Model: "glm-5.3-flash", ReasoningEffort: level}
		assert.Empty(t, staleEffortNote(ok), "level %q is valid and must not be flagged", level)
		assert.Empty(t, effortEffectiveNote(ok), "level %q is valid and must not be flagged", level)
	}

	// Efforts on models outside the atom registry stay deliberately
	// unvalidated — no false "stale" claim for them.
	nonAtom := config.SelectedModel{Provider: "openai", Model: "gpt-5", ReasoningEffort: "whatever"}
	assert.Empty(t, staleEffortNote(nonAtom))

	// Unset effort is never stale.
	assert.Empty(t, staleEffortNote(config.SelectedModel{Provider: "zai", Model: "glm-5.3-flash"}))
}

// TestNilOrStaleEffort_JSONCounterpart mirrors the text-mode test above for
// the --json path, which reports the valid levels so an orchestrator can
// repair a stale slot without re-deriving the atom's vocabulary.
func TestNilOrStaleEffort_JSONCounterpart(t *testing.T) {
	stale := config.SelectedModel{Provider: "zai", Model: "glm-5.3-flash", ReasoningEffort: "on"}
	assert.Equal(t, []string{"low", "high", "max"}, nilOrStaleEffort(true, stale))

	valid := config.SelectedModel{Provider: "zai", Model: "glm-5.3-flash", ReasoningEffort: "max"}
	assert.Nil(t, nilOrStaleEffort(true, valid))

	// Slot not set at all: nil even when the stored effort would be stale.
	assert.Nil(t, nilOrStaleEffort(false, stale))
}

// TestStaleEffortNote_ClaudeAtomNeverShellsOut is the regression test for a
// bug a code review caught in the GLM-5.3 stale-effort fix: staleEffortNote
// used to call validateEffortForModel unconditionally, which for a
// Claude/local-cli atom shells out to `claude --help` (models_effort.go's
// cliEffortSource.Levels()) to discover its levels. That made `rush models
// state` — a read-only status command — spawn a subprocess (with no timeout,
// under a package-level mutex) as a side effect of rendering a line, on
// EVERY invocation where a Claude slot has an explicit effort set.
//
// The deliberately unknown effort must not be flagged as stale, regardless of
// the installed CLI vocabulary or the fallback levels.
func TestStaleEffortNote_ClaudeAtomNeverShellsOut(t *testing.T) {
	claudeAtom := config.SelectedModel{Provider: "local-cli", Model: "cli-claude-opus-4-8", ReasoningEffort: "legacy-cli-level"}
	assert.Empty(t, staleEffortNote(claudeAtom), "Claude/local-cli atoms must never be flagged stale — their vocabulary is CLI-detected, not a static declaration")
	assert.Empty(t, effortEffectiveNote(claudeAtom))
}
