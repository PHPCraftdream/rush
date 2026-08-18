// `crush models state` tests: worker/reviewer reporting in text and --json
// modes, unset-effort default notes, and unit tests of the note helpers
// (unsetEffortNote, effortEffectiveNote, nilOrEffortDefault).
package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/crush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelsState_ReportsWorkerAndReviewer(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd)
	require.NoError(t, runErr)

	assert.Contains(t, out, "worker:")
	assert.Contains(t, out, "glm-4.7-flash")
	assert.Contains(t, out, "reviewer:")
	assert.Contains(t, out, "glm-5.2")
}

func TestModelsState_JSONReportsWorkerAndReviewer(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd,
		"glm5_1", "glm5_turbo", "--worker", "glm4_7_flash", "--reviewer", "glm5_2")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)

	assert.Contains(t, out, `"worker"`)
	assert.Contains(t, out, `"reviewer"`)
	assert.Contains(t, out, "glm-4.7-flash")
	assert.Contains(t, out, "glm-5.2")
}

// TestModelsState_UnsetEffort_ShowsKnownZAIDefault covers the core case for
// this task: a slot with no explicit effort on a provider with a KNOWN
// documented unset-default (Z.AI, "unset -> thinking on, high" per
// coordinator.go's getProviderOptions and providerEffortDocs in
// models_efforts.go) must show that default as a terse parenthetical.
// "glm5_1" (large, in this test) is set with no @effort suffix, so
// m.ReasoningEffort == "" and effortEffectiveNote must fire.
func TestModelsState_UnsetEffort_ShowsKnownZAIDefault(t *testing.T) {
	isolatedModelsEnv(t)

	resetModelsUseFlags(t)
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo")
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
// glm5_2: the embedded catwalk provider catalog this test env falls back to
// in CRUSH_PROVIDER_CACHE_ONLY mode doesn't list glm-5.2 at all, which makes
// config's large/small validation silently substitute a different zai model
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

	largeLine, ok := lineContaining(out, "large:")
	require.True(t, ok, "expected a 'large:' line in output:\n%s", out)
	assert.Contains(t, largeLine, "effort=on")
	assert.NotContains(t, largeLine, "unset ->")
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
	_, runErr := runModelsCmd(t, modelsUseCmd, "glm5_1", "glm5_turbo")
	require.NoError(t, runErr)

	resetModelsStateFlags(t)
	out, runErr := runModelsCmd(t, modelsStateCmd, "--json")
	require.NoError(t, runErr)

	var doc struct {
		Effective struct {
			LargeEffortDefault *string `json:"large_effort_default"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	require.NotNil(t, doc.Effective.LargeEffortDefault)
	assert.Equal(t, "unset -> thinking on, high", *doc.Effective.LargeEffortDefault)
}

// TestModelsState_JSON_ExplicitEffort_NullDefault verifies the JSON default
// field is null (not the fact string) when the slot has an explicit effort —
// the machine-readable mirror of the text-mode precedence test above. See
// the comment on TestModelsState_ExplicitEffort_TakesPrecedenceOverDefault
// for why glm4_7_flash is used instead of glm5_2 here.
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
			LargeEffortDefault *string `json:"large_effort_default"`
		} `json:"effective"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.Nil(t, doc.Effective.LargeEffortDefault)
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
	explicit := config.SelectedModel{Provider: "zai", Model: "glm-5.2", ReasoningEffort: "max"}
	assert.Empty(t, effortEffectiveNote(explicit))

	// Unset effort, known Z.AI default: note must appear.
	unsetZAI := config.SelectedModel{Provider: "zai", Model: "glm-5.2"}
	assert.Contains(t, effortEffectiveNote(unsetZAI), "unset -> thinking on, high")

	// Unset effort, undocumented provider: must stay silent, not guess.
	unsetUnknown := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	assert.Empty(t, effortEffectiveNote(unsetUnknown))
}

// TestNilOrEffortDefault_JSONCounterpart mirrors the text-mode test above
// for the --json path's nilOrEffortDefault helper.
func TestNilOrEffortDefault_JSONCounterpart(t *testing.T) {
	explicit := config.SelectedModel{Provider: "zai", Model: "glm-5.2", ReasoningEffort: "max"}
	assert.Nil(t, nilOrEffortDefault(true, explicit))

	unsetZAI := config.SelectedModel{Provider: "zai", Model: "glm-5.2"}
	assert.Equal(t, "unset -> thinking on, high", nilOrEffortDefault(true, unsetZAI))

	unsetUnknown := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	assert.Nil(t, nilOrEffortDefault(true, unsetUnknown))

	// Slot not set at all: nil regardless of provider/effort.
	assert.Nil(t, nilOrEffortDefault(false, unsetZAI))
}
