package cmd

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/stretchr/testify/require"
)

var cahLadder = []string{"low", "medium", "high", "xhigh", "max", "ultra"}

var cahVersionRe = regexp.MustCompile(`(\d+)(?:[.-](\d+))?`)

// cahModelVersion parses the first numeric version (major, minor) from a model name.
func cahModelVersion(model string) [2]int {
	m := cahVersionRe.FindStringSubmatch(model)
	if m == nil {
		return [2]int{}
	}
	var v [2]int
	v[0], _ = strconv.Atoi(m[1])
	v[1], _ = strconv.Atoi(m[2])
	return v
}

func cahSpecBySlug() map[string]cliprovider.CLISpec {
	out := map[string]cliprovider.CLISpec{}
	for _, spec := range cliprovider.All {
		out[spec.ModelID] = spec
	}
	return out
}

// cahFamilyLetter extracts the family letter: first char for Claude, after the effort prefix for Codex.
func cahFamilyLetter(code, binary string) string {
	if binary == "claude" {
		return code[:1]
	}
	rest := code[1:]
	if strings.HasPrefix(code, "xx") {
		rest = code[2:]
	}
	return rest[:1]
}

func sortedCAHCodes() []string {
	codes := make([]string, 0, len(cahNameMap))
	for code := range cahNameMap {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// Revert-check: exact generated codes must win over historical top aliases.
func TestCAHExactCodes(t *testing.T) {
	for code, r := range cahNameMap {
		t.Run(code, func(t *testing.T) {
			sm, err := parseAtom(code)
			require.NoError(t, err)
			require.Equal(t, r.Provider, sm.Provider)
			require.Equal(t, r.Slug, sm.Model)
			require.Equal(t, r.Effort, sm.ReasoningEffort)
		})
	}
}

// Revert-check: removed legacy version codes are rejected before any raw or atom-prefix resolution.
func TestCAHLegacyCodesRejected(t *testing.T) {
	resolve := func(string) (string, string, bool, error) {
		t.Fatal("raw resolver must not be called for a legacy code")
		return "", "", false, nil
	}
	for _, code := range []string{"o47x", "s46xx", "h45l", "o48xx", "s45h", "o46h"} {
		t.Run(code, func(t *testing.T) {
			_, _, err := parseAtomOrRaw(code, resolve)
			require.Error(t, err)
			require.Contains(t, err.Error(), "cc-arch-hands")
			require.Contains(t, err.Error(), "rush models list")
			_, ok := parseShortCode(code)
			require.False(t, ok)
		})
	}
}

// Revert-check: a digit-less (top) code is the newest model of its binary, family letter and effort.
func TestCAHTopCodeIsNewest(t *testing.T) {
	specs := cahSpecBySlug()
	type key struct{ binary, family, effort string }
	newest := map[key][2]int{}
	keyOf := func(code string) key {
		r := cahNameMap[code]
		spec, ok := specs[r.Slug]
		require.True(t, ok, "%s: no spec for %s", code, r.Slug)
		return key{spec.Binary, cahFamilyLetter(code, spec.Binary), r.Effort}
	}
	for code, r := range cahNameMap {
		k := keyOf(code)
		if v := cahModelVersion(r.Model); newest[k] == [2]int{} || v[0] > newest[k][0] || (v[0] == newest[k][0] && v[1] > newest[k][1]) {
			newest[k] = v
		}
	}
	tops := 0
	for code, r := range cahNameMap {
		if strings.ContainsAny(code, "0123456789") {
			continue
		}
		tops++
		require.Equal(t, newest[keyOf(code)], cahModelVersion(r.Model), "top code %s (%s) is not the newest in its group", code, r.Model)
	}
	require.Positive(t, tops)
}

// Revert-check: every table code resolves through the production resolver to a real spec and legal effort.
func TestCAHEveryCodeRealResolver(t *testing.T) {
	isolatedModelsEnv(t)
	original := cliprovider.AvailableFunc
	cliprovider.AvailableFunc = func() []cliprovider.CLISpec { return cliprovider.All }
	t.Cleanup(func() { cliprovider.AvailableFunc = original })
	a, err := setupAppLite(modelsUseCmd)
	require.NoError(t, err)
	defer a.Shutdown()
	binaries := map[string]bool{}
	for code, r := range cahNameMap {
		t.Run(code, func(t *testing.T) {
			sm, known, err := parseAtomOrRaw(code, a.ResolveModel)
			require.NoError(t, err)
			require.True(t, known)
			provider, id, known, err := a.ResolveModel(sm.Provider + "/" + sm.Model)
			require.NoError(t, err)
			require.True(t, known)
			require.Equal(t, r.Provider, provider)
			require.Equal(t, r.Slug, id)
			found := false
			for _, spec := range cliprovider.All {
				if spec.ModelID != id {
					continue
				}
				found = true
				binaries[spec.Binary] = true
				require.Contains(t, spec.EffortLevels, sm.ReasoningEffort)
				require.NoError(t, validateEffortForModel(provider, id, sm.ReasoningEffort))
				require.Equal(t, r.Context, spec.ContextWindow)
			}
			require.True(t, found)
		})
	}
	require.True(t, binaries["claude"])
	require.True(t, binaries["codex"])
	// A syntactically valid short code absent from the table must fail before raw resolution.
	invalid := ""
	for _, c := range []string{"o9x", "s9xx", "h9l", "f9m", "xs9", "lt9"} {
		if _, ok := cahNameMap[c]; !ok {
			invalid = c
			break
		}
	}
	require.NotEmpty(t, invalid)
	_, _, err = parseAtomOrRaw(invalid, func(string) (string, string, bool, error) {
		t.Fatal("invalid code reached raw resolution")
		return "", "", false, nil
	})
	require.ErrorContains(t, err, "invalid short code")
	// Raw local-cli/<id>@<level> accepts exactly each spec's own levels.
	slugs := map[string]bool{}
	for _, r := range cahNameMap {
		slugs[r.Slug] = true
	}
	for _, spec := range cliprovider.All {
		if !slugs[spec.ModelID] {
			continue
		}
		for _, level := range spec.EffortLevels {
			sm, known, err := parseAtomOrRaw("local-cli/"+spec.ModelID+"@"+level, a.ResolveModel)
			require.NoError(t, err, spec.ModelID+"@"+level)
			require.True(t, known)
			require.Equal(t, level, sm.ReasoningEffort)
		}
		for _, level := range cahLadder {
			if slices.Contains(spec.EffortLevels, level) {
				continue
			}
			_, _, err := parseAtomOrRaw("local-cli/"+spec.ModelID+"@"+level, a.ResolveModel)
			require.ErrorContains(t, err, "not a valid effort", spec.ModelID+"@"+level)
			break
		}
	}
	// Bare atom@effort keeps its existing raw fallback, not atom suffix parsing.
	_, _, err = parseAtomOrRaw("opus@high", a.ResolveModel)
	require.Error(t, err)
}

// Revert-check: help is rendered from the live sorted table and cahNotes, including future entries and version footer.
func TestCAHHelpFromTable(t *testing.T) {
	const extra = "zztest"
	const extraNote = "zz-extra-note-marker"
	cahNameMap[extra] = cahName{"local-cli", "test-slug", "test-effort", "test-model", "test-display", 123000}
	defer delete(cahNameMap, extra)
	top := sortedCAHCodes()[0]
	oldTop := cahNameMap[top]
	cahNameMap[top] = cahName{"local-cli", "future-model", "xhigh", "future-model", "Future Model", 123000}
	defer func() { cahNameMap[top] = oldTop }()
	oldNotes := cahNotes
	cahNotes = append(append([]string{}, oldNotes...), extraNote)
	defer func() { cahNotes = oldNotes }()
	out := renderShortCodesBlock()
	require.Contains(t, out, "future-model")
	require.NotContains(t, out, "o47x")
	require.NotContains(t, out, "o48")
	for _, note := range cahNotes {
		require.Contains(t, out, "  "+note+"\n")
	}
	require.Contains(t, out, extraNote)
	require.Contains(t, out, "generated from cc-arch-hands "+cahVersion)
	require.NotContains(t, out, "Deprecated legacy")
	last := ""
	for _, line := range strings.Split(strings.SplitN(out, "  -------", 2)[1], "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		r, ok := cahNameMap[fields[0]]
		if !ok {
			continue
		}
		require.Greater(t, fields[0], last)
		last = fields[0]
		require.Equal(t, []string{fields[0], r.Model, humanCtx(r.Context), r.Effort}, fields)
	}
	require.Equal(t, extra, last)
	require.Contains(t, out, "test-model")
	require.Contains(t, out, "test-effort")
}

// Revert-check: generated headers retain provenance; Codex help never uses Claude detection or semantics.
func TestCAHHeadersAndEfforts(t *testing.T) {
	data, err := os.ReadFile("models_cah_gen.go")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(data), "// Code generated by cahsync from cc-arch-hands "+cahVersion+"; DO NOT EDIT.\n"))
	specs := cahSpecBySlug()
	var claude, codex, fewest string
	for _, code := range sortedCAHCodes() {
		spec := specs[cahNameMap[code].Slug]
		switch spec.Binary {
		case "claude":
			if claude == "" {
				claude = code
			}
		case "codex":
			if codex == "" {
				codex = code
			}
			if fewest == "" || len(spec.EffortLevels) < len(specs[cahNameMap[fewest].Slug].EffortLevels) {
				fewest = code
			}
		}
	}
	require.NotEmpty(t, claude)
	require.NotEmpty(t, codex)
	for _, code := range []string{claude, codex, fewest} {
		out, err := renderEffortsForModel(code)
		require.NoError(t, err)
		spec := specs[cahNameMap[code].Slug]
		for _, level := range spec.EffortLevels {
			require.Contains(t, out, "@"+level+" <fast>", code)
		}
		for _, level := range cahLadder {
			if !slices.Contains(spec.EffortLevels, level) {
				require.NotContains(t, out, "@"+level+" <fast>", code)
			}
		}
		if spec.Binary == "codex" {
			require.Contains(t, out, "model_reasoning_effort")
			require.NotContains(t, out, "Claude")
			require.NotContains(t, out, "claude --help")
		}
	}
}
