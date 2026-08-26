// `rush models state` tests: worker/reviewer reporting in text and --json
// modes, unset-effort default notes, and unit tests of the note helpers
// (unsetEffortNote, effortEffectiveNote, nilOrEffortDefault).
package cmd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelsState_ReportsWorkerAndReviewer(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_turbo", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)

	assert.Contains(t, out, "worker:")
	assert.Contains(t, out, "glm-4.7-flash")
	assert.Contains(t, out, "reviewer:")
	assert.Contains(t, out, "glm-5.3")
}

func TestModelsState_JSONReportsWorkerAndReviewer(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_turbo", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_3")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
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
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_turbo", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd)
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
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-on", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)

	smartLine, ok := lineContaining(out, "smart:")
	require.True(t, ok, "expected a 'large:' line in output:\n%s", out)
	assert.Contains(t, smartLine, "effort=on")
	assert.NotContains(t, smartLine, "unset ->")
}

// lineContaining returns the first line of s containing substr, and whether
// one was found.
func lineContaining(s, substr string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			return line, true
		}
	}
	return "", false
}

// TestModelsState_JSON_UnsetEffort_ShowsKnownDefault verifies the --json
// mode also surfaces the same fact machine-readably (nilOrEffortDefault),
// not just the human text rendering.
func TestModelsState_JSON_UnsetEffort_ShowsKnownDefault(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_turbo", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
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
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm4_7_flash-on", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)

	var doc struct {
		Effective struct {
			SmartEffortDefault *string `json:"smart_effort_default"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.Nil(t, doc.Effective.SmartEffortDefault)
}

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
	assert.Equal(t, "unset -> thinking on, high", unsetEffortNote("zai"))
	assert.Equal(t, "unset -> thinking off", unsetEffortNote("deepseek"))

	for _, provider := range []string{"ionet", "hyper", "alibaba-singapore", "openai", "anthropic", "local-cli"} {
		assert.Empty(t, unsetEffortNote(provider), "provider %q has no documented unset-default fact and must not guess one", provider)
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
// This does NOT use setFallbackEffortSource/setMockEffortLevels: a code
// review caught that those helpers only reassign the package-level
// `claudeEffortSource` variable, while every atomRegistry entry's
// `EffortSource` field already captured the ORIGINAL pointer value at
// package-init time — a mock swap in a test never reaches an atom looked up
// via atomRegistry, so it would give false confidence here without actually
// making the test hermetic (a regression on a machine with `claude`
// installed would still shell out to the real binary). Using
// "legacy-cli-level" — a string outside BOTH the real CLI's plausible output
// and the hardcoded fallback list — makes the assertion fail on a regression
// regardless of which one actually gets consulted, without pretending a
// mock prevents that consultation from happening at all.
func TestStaleEffortNote_ClaudeAtomNeverShellsOut(t *testing.T) {
	claudeAtom := config.SelectedModel{Provider: "local-cli", Model: "cli-claude-opus-4-8", ReasoningEffort: "legacy-cli-level"}
	assert.Empty(t, staleEffortNote(claudeAtom), "Claude/local-cli atoms must never be flagged stale — their vocabulary is CLI-detected, not a static declaration")
	assert.Empty(t, effortEffectiveNote(claudeAtom))
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
	globalPath := isolatedModelsEnv(t)

	require.NoError(t, os.WriteFile(globalPath, []byte(`{
		"providers": {"zai": {"api_key": "test-zai-key"}},
		"models": {
			"smart": {"provider": "zai", "model": "glm-5.3-flash", "reasoning_effort": "on"},
			"fast": {"provider": "zai", "model": "glm-5-turbo"}
		}
	}`), 0o644))

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)

	smartLine, ok := lineContaining(out, "smart:")
	require.True(t, ok, "expected a 'smart:' line in output:\n%s", out)
	assert.Contains(t, smartLine, "(STALE:")
	assert.Contains(t, smartLine, `"on"`)
	assert.Contains(t, smartLine, "low|high|max")
	assert.NotContains(t, smartLine, "atom: glm5_3_flash-on",
		"must never render a <atom>-<effort> form that `rush models use` would now reject")

	// --json must carry the same fact machine-readably.
	resetModelsStateFlags(t)
	jsonOut, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)
	assert.Contains(t, jsonOut, `"smart_effort_stale":["low","high","max"]`)
}
