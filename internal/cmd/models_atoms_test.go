package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStubResolve = errors.New("stub resolve")

func noopResolve(s string) (string, string, bool, error) {
	return "", "", false, errStubResolve
}

// TestAtom_ZAIHasReasoningLevels verifies every Z.AI atom carries a real,
// declared levels array (not just prose describing them). glm5_3 and
// glm5_3_flash get exactly the three real states Z.AI's API actually
// accepts for these two models — "low","high","max" (verified 2026-08-26;
// disabling reasoning is rejected by the API for these two specifically,
// unlike every older Z.AI model), and every other Z.AI atom gets the
// boolean thinking-toggle-only array. Also checks atom.Levels() surfaces
// each.
func TestAtom_ZAIHasReasoningLevels(t *testing.T) {
	for _, key := range []string{"glm5_3", "glm5_3_flash"} {
		a, ok := atomRegistry[key]
		require.True(t, ok, "expected atom %q in registry", key)
		assert.Equal(t, []string{"low", "high", "max"}, a.ReasoningLevels, "atom %q", key)
		assert.Nil(t, a.EffortSource, "Z.AI atom %q must not use the CLI-shelling EffortSource", key)
		assert.Equal(t, a.ReasoningLevels, a.Levels(), "atom %q .Levels()", key)
	}

	for _, key := range []string{"glm5_turbo", "glm4_7", "glm4_7_flash", "glm4_6", "glm4_6v"} {
		a, ok := atomRegistry[key]
		require.True(t, ok, "expected atom %q in registry", key)
		assert.Equal(t, []string{"off", "on"}, a.ReasoningLevels, "atom %q — only GLM-5.3/5.3-Flash have graduated effort per Z.AI docs", key)
		assert.Nil(t, a.EffortSource, "Z.AI atom %q must not use the CLI-shelling EffortSource", key)
		assert.Equal(t, []string{"off", "on"}, a.Levels(), "atom %q .Levels()", key)
	}
}

// TestAtom_ClaudeUnaffectedByReasoningLevels is a regression check: adding
// ReasoningLevels to the atom struct must not change Claude/local-cli atoms,
// which continue to source their levels exclusively from EffortSource (the
// CLI-shelling mechanism) and carry no static ReasoningLevels of their own.
func TestAtom_ClaudeUnaffectedByReasoningLevels(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high", "xhigh", "max"})()
	for _, key := range []string{"opus", "opus46", "opus47", "opus48", "sonnet", "haiku", "fable"} {
		a, ok := atomRegistry[key]
		require.True(t, ok, "expected atom %q in registry", key)
		assert.Nil(t, a.ReasoningLevels, "Claude atom %q should not carry a static ReasoningLevels", key)
		require.NotNil(t, a.EffortSource, "Claude atom %q must use EffortSource", key)
		assert.Equal(t, []string{"low", "medium", "high", "xhigh", "max"}, a.Levels(), "atom %q .Levels()", key)
	}
}

func TestParseAtom_AnthropicWithLevel(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high"})()
	sm, err := parseAtom("opus-high")
	require.NoError(t, err)
	assert.Equal(t, "local-cli", sm.Provider)
	// `opus` is now an alias for the latest pinned Opus version (4.8).
	// Earlier atoms pointed at the legacy "cli-claude-opus" alias entry;
	// we kept the registry pointing at the pinned id so newly-created
	// sessions track the freshest generation.
	assert.Equal(t, "cli-claude-opus-4-8", sm.Model)
	assert.Equal(t, "high", sm.ReasoningEffort)
}

func TestParseAtom_AnthropicMissingLevel(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high"})()
	_, err := parseAtom("opus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires explicit level")
}

func TestParseAtom_AnthropicInvalidLevel(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high"})()
	_, err := parseAtom("opus-blah")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid level")
}

func TestParseAtom_ZAINoLevel(t *testing.T) {
	sm, err := parseAtom("glm5_turbo")
	require.NoError(t, err)
	assert.Equal(t, "zai", sm.Provider)
	assert.Equal(t, "glm-5-turbo", sm.Model)
	assert.Empty(t, sm.ReasoningEffort)
}

// TestParseAtom_ZAIRejectsLevel previously asserted that ANY "-level" suffix
// on a Z.AI atom was rejected outright ("does not support effort levels"),
// because Z.AI atoms carried no machine-readable levels array at all — only
// the raw, unvalidated "provider/model@effort" syntax could set a Z.AI
// model's effort.
//
// That contract has deliberately changed (task: give every atom a real
// ReasoningLevels array). glm5_turbo (like every non-graduated Z.AI atom —
// everything except glm5_3 and glm5_3_flash) declares {"off", "on"} — Z.AI's
// own docs only document a boolean thinking toggle
// for this model, not graduated reasoning_effort — and the long-form
// "<atom>-<level>" suffix is validated against that array exactly like
// Claude atoms are validated against EffortSource.Levels(). "low" is not one
// of glm5_turbo's two valid values, so it is still rejected — but now with the
// more precise "not a valid level" error (listing the real off|on
// vocabulary) instead of the old blanket "does not support effort levels"
// message. See TestParseAtom_ZAIAcceptsKnownLevel below for the
// now-succeeding case this test used to make impossible, and
// TestParseAtom_ZAI53AcceptsGraduatedLevel for glm5_3's distinct, larger
// vocabulary.
func TestParseAtom_ZAIRejectsLevel(t *testing.T) {
	_, err := parseAtom("glm5_turbo-low")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "not a valid level")
	assert.Contains(t, err.Error(), "off|on")
}

// TestParseAtom_ZAIAcceptsKnownLevel is the new behavior enabled by giving
// Z.AI atoms a real ReasoningLevels array: a level that IS one of glm5_turbo's
// two valid boolean-toggle values (off/on) is now accepted via the same
// "<atom>-<level>" long-form suffix Claude atoms already use, instead of
// requiring the raw unvalidated "zai/glm-5-turbo@on" syntax.
func TestParseAtom_ZAIAcceptsKnownLevel(t *testing.T) {
	sm, err := parseAtom("glm5_turbo-on")
	require.NoError(t, err)
	assert.Equal(t, "zai", sm.Provider)
	assert.Equal(t, "glm-5-turbo", sm.Model)
	assert.Equal(t, "on", sm.ReasoningEffort)
}

// TestParseAtom_ZAI53AcceptsGraduatedLevel proves glm5_3 specifically
// validates against its own, larger vocabulary — a level like "max" that
// would be invalid for glm5_turbo (off/on only) is valid here.
func TestParseAtom_ZAI53AcceptsGraduatedLevel(t *testing.T) {
	sm, err := parseAtom("glm5_3-max")
	require.NoError(t, err)
	assert.Equal(t, "zai", sm.Provider)
	assert.Equal(t, "glm-5.3", sm.Model)
	assert.Equal(t, "max", sm.ReasoningEffort)
}

// TestParseAtom_ZAI53RejectsWiderVendorVocabulary proves glm5_3 only accepts
// its own real, documented reasoning_effort enum (low/high/max) and rejects
// both the wider Z.AI-documented vocabulary that this fork's coordinator
// doesn't produce ("xhigh", "none", "minimal", "medium") AND the old
// off/high/max shape from before the 2026-08-26 correction ("off" is no
// longer valid for glm5_3 — the API rejects disabling reasoning for it).
func TestParseAtom_ZAI53RejectsWiderVendorVocabulary(t *testing.T) {
	for _, level := range []string{"off", "xhigh", "none", "minimal", "medium"} {
		t.Run(level, func(t *testing.T) {
			_, err := parseAtom("glm5_3-" + level)
			require.Error(t, err)
			assert.Contains(t, strings.ToLower(err.Error()), "not a valid level")
			assert.Contains(t, err.Error(), "low|high|max")
		})
	}
}

// TestParseAtom_ZAITypoRejected proves the exact gap the task description
// calls out: a typo'd effort level (here "hgih" instead of "high") is now
// rejected with a clear error, instead of silently succeeding the way the
// unvalidated raw "@effort" suffix does (see splitModelEffort/models_use.go).
func TestParseAtom_ZAITypoRejected(t *testing.T) {
	_, err := parseAtom("glm5_3-hgih")
	require.Error(t, err)
	assert.Contains(t, strings.ToLower(err.Error()), "not a valid level")
}

// TestParseAtom_LongestMatchWins guards sortedAtomKeys' longest-first order:
// when one atom key is a strict prefix of another, the parser must pick the
// longer one rather than greedily matching the shorter and treating the
// remainder as an effort suffix.
//
// This used to be exercised with glm5 / glm5_1 / glm5_turbo. glm5 and glm5_1
// were removed from the registry on 2026-08-18, so that trio no longer
// demonstrates anything — "glm5" is not an atom at all now. The hazard itself
// is unchanged and still live for glm4_7 vs glm4_7_flash, which is what this
// test now covers. Without longest-first ordering, "glm4_7_flash" would match
// atom "glm4_7" and try to parse "_flash" as a level.
func TestParseAtom_LongestMatchWins(t *testing.T) {
	sm, err := parseAtom("glm4_7_flash")
	require.NoError(t, err)
	assert.Equal(t, "glm-4.7-flash", sm.Model)

	sm2, err := parseAtom("glm4_7")
	require.NoError(t, err)
	assert.Equal(t, "glm-4.7", sm2.Model)

	sm3, err := parseAtom("glm5_turbo")
	require.NoError(t, err)
	assert.Equal(t, "glm-5-turbo", sm3.Model)
}

func TestParseAtom_UnknownAtom(t *testing.T) {
	_, err := parseAtom("totally-fake-model")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a recognized atom")
}

func TestParseAtomOrRaw_FallsBackToProviderSlashModel(t *testing.T) {
	resolve := func(s string) (string, string, bool, error) {
		assert.Equal(t, "openai/gpt-5", s)
		return "openai", "gpt-5", true, nil
	}
	sm, known, err := parseAtomOrRaw("openai/gpt-5@high", resolve)
	require.NoError(t, err)
	assert.Equal(t, "openai", sm.Provider)
	assert.Equal(t, "gpt-5", sm.Model)
	assert.Equal(t, "high", sm.ReasoningEffort)
	assert.True(t, known)
}

func TestParseAtomOrRaw_AtomWinsOverFallback(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "high"})()
	called := false
	resolve := func(s string) (string, string, bool, error) {
		called = true
		return "", "", false, nil
	}
	sm, known, err := parseAtomOrRaw("opus-high", resolve)
	require.NoError(t, err)
	assert.False(t, called, "resolver must NOT be called when atom matches")
	assert.Equal(t, "local-cli", sm.Provider)
	assert.Equal(t, "cli-claude-opus-4-8", sm.Model)
	assert.True(t, known, "atoms are always known")
}

func TestParseAtomOrRaw_UnknownAtomAndNoSlash(t *testing.T) {
	_, _, err := parseAtomOrRaw("nonsense", noopResolve)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a known atom or provider/model")
}

// TestParseAtomOrRaw_UnverifiedPassthroughReportsUnknown pins the new
// findModels/ResolveModel fallback contract (2026-08-26 feature request):
// an explicit provider/model that the resolver couldn't verify against a
// catalog still succeeds, but must report known=false so callers (e.g.
// `rush models use`) can warn the operator instead of silently accepting a
// possible typo.
func TestParseAtomOrRaw_UnverifiedPassthroughReportsUnknown(t *testing.T) {
	resolve := func(s string) (string, string, bool, error) {
		assert.Equal(t, "zai/glm-5.3-flash", s)
		return "zai", "glm-5.3-flash", false, nil
	}
	sm, known, err := parseAtomOrRaw("zai/glm-5.3-flash", resolve)
	require.NoError(t, err)
	assert.Equal(t, "zai", sm.Provider)
	assert.Equal(t, "glm-5.3-flash", sm.Model)
	assert.False(t, known, "an unverified provider/model passthrough must report known=false")
}

func TestLookupAtomForModel(t *testing.T) {
	k := lookupAtomForModel(config.SelectedModel{Provider: "local-cli", Model: "cli-claude-opus-4-8"})
	// Both "opus" (alias to latest) and "opus48" (explicit pin) resolve to
	// the same underlying model id; lookupAtomForModel returns whichever
	// key the registry walk reaches first, so accept either.
	assert.Contains(t, []string{"opus", "opus48"}, k)
	k2 := lookupAtomForModel(config.SelectedModel{Provider: "zai", Model: "glm-5-turbo"})
	assert.Equal(t, "glm5_turbo", k2)
	k3 := lookupAtomForModel(config.SelectedModel{Provider: "openai", Model: "gpt-5"})
	assert.Empty(t, k3, "non-atom model returns empty string")
}

// (splitModelEffort is covered by helpers_test.go — don't duplicate.)

func TestParseShortCode_Valid(t *testing.T) {
	cases := []struct {
		code   string
		model  string
		effort string
	}{
		{"o48h", "cli-claude-opus-4-8", "high"},
		{"o48xx", "cli-claude-opus-4-8", "max"},
		{"o47h", "cli-claude-opus-4-7", "high"},
		{"o47xx", "cli-claude-opus-4-7", "max"},
		{"o47x", "cli-claude-opus-4-7", "xhigh"},
		{"o46xx", "cli-claude-opus-4-6", "max"},
		{"s46h", "cli-claude-sonnet", "high"},
		{"s45h", "cli-claude-sonnet", "high"},
		{"h45l", "cli-claude-haiku", "low"},
		{"oh", "cli-claude-opus-4-8", "high"},
		{"sl", "cli-claude-sonnet", "low"},
		{"hm", "cli-claude-haiku", "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			sm, ok := parseShortCode(tc.code)
			require.True(t, ok, "expected ok for %s", tc.code)
			assert.Equal(t, "local-cli", sm.Provider)
			assert.Equal(t, tc.model, sm.Model)
			assert.Equal(t, tc.effort, sm.ReasoningEffort)
		})
	}
}

func TestParseShortCode_Invalid(t *testing.T) {
	invalid := []string{"o47-3", "o47-0", "s45xx", "h45x", "x47h", "o99h", "", "opus-high"}
	for _, code := range invalid {
		_, ok := parseShortCode(code)
		assert.False(t, ok, "expected not-ok for %q", code)
	}
}

func TestParseAtom_ShortCodeRoundtrip(t *testing.T) {
	defer setMockEffortLevels([]string{"low", "medium", "high", "xhigh", "max"})()
	sm, err := parseAtom("o47x")
	require.NoError(t, err)
	assert.Equal(t, "local-cli", sm.Provider)
	assert.Equal(t, "cli-claude-opus-4-7", sm.Model)
	assert.Equal(t, "xhigh", sm.ReasoningEffort)
}
