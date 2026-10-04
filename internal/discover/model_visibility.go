// The single model-visibility table: which model families are hidden from
// every list (CLI listings, the web picker, provider catalogs and their
// caches) because the provider superseded them. One predicate, one table —
// do not add per-surface filtering elsewhere.
package discover

import (
	"slices"
	"strconv"
	"strings"
)

// modelVisibilityRule hides one model family whose ids carry a version
// number below a documented minimum, or a retired family with no usable
// version scheme at all.
type modelVisibilityRule struct {
	// providers limits the rule to these provider IDs; empty means every
	// provider.
	providers []string
	// family is the model-id token the rule matches, e.g. "gpt" or "glm".
	// It matches at the start of the id or right after a "-" separator, and
	// must be followed by "-" or end the id, so "cc-glm-5.1" matches "glm"
	// while "gptx-1" does not match "gpt".
	family string
	// minVersion is the lowest version the family still shows. An id whose
	// version cannot be parsed stays visible so an unknown model is never
	// lost.
	minVersion float64
	// hideAll hides the whole family regardless of version. For legacy
	// families whose ids carry no version number (o3, codex-mini).
	hideAll bool
}

// modelVisibilityRules is the family-to-minimum-version table.
// openai-codex shows gpt-6 and newer; glm models are hidden below 5.3 for
// every provider. Everything else is visible.
var modelVisibilityRules = [...]modelVisibilityRule{
	{providers: []string{"openai-codex"}, family: "gpt", minVersion: 6},
	{providers: []string{"openai-codex"}, family: "o3", hideAll: true},
	{providers: []string{"openai-codex"}, family: "codex-mini", hideAll: true},
	{family: "glm", minVersion: 5.3},
}

// ModelVisible reports whether a model id belongs in provider's lists.
// Ids outside the table, and ids inside a versioned family whose version
// cannot be parsed, are always visible: the filter must never lose an
// unknown model.
func ModelVisible(provider, id string) bool {
	for _, rule := range modelVisibilityRules {
		if len(rule.providers) > 0 && !slices.Contains(rule.providers, provider) {
			continue
		}
		version, parsed, found := familyVersion(id, rule.family)
		if !found {
			continue
		}
		if rule.hideAll || parsed && version < rule.minVersion {
			return false
		}
	}
	return true
}

// familyVersion looks for family in id at a "-" boundary and parses the
// dotted version number that follows it. found reports whether the family
// token is present; parsed reports whether a version number follows it
// (suffixes like "-flash", "-turbo" or "v" do not change the version).
func familyVersion(id, family string) (version float64, parsed, found bool) {
	for start := 0; start < len(id); {
		index := strings.Index(id[start:], family)
		if index < 0 {
			return 0, false, false
		}
		index += start
		if index == 0 || id[index-1] == '-' {
			rest := index + len(family)
			if rest == len(id) {
				return 0, false, true
			}
			if id[rest] == '-' {
				return parseLeadingVersion(id[rest+1:])
			}
		}
		start = index + 1
	}
	return 0, false, false
}

// parseLeadingVersion parses the leading dotted decimal of s. found is
// always true (the family matched); parsed reports whether s starts with a
// number, so "6-luna" parses as 6 while "x" does not parse at all.
func parseLeadingVersion(s string) (version float64, parsed, found bool) {
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false, true
	}
	if end < len(s) && s[end] == '.' {
		dot := end + 1
		for dot < len(s) && s[dot] >= '0' && s[dot] <= '9' {
			dot++
		}
		if dot > end+1 {
			end = dot
		}
	}
	value, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return 0, false, true
	}
	return value, true, true
}
