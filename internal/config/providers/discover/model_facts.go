// Package-level single source of documented model metadata: the context
// window and default max output of the model families whose values are
// documented by their vendor (or asserted by the operator). Every other copy
// of these numbers was removed; ApplyModelFacts is the only function that
// applies them, and the final pass at the end of config's loadProviders is
// the only call site. Nothing may change ContextWindow or DefaultMaxTokens
// after that pass.
package discover

import (
	"slices"
	"strconv"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

const (
	// codexDefaultContextWindow is the documented window of an
	// openai-codex model without a more specific row.
	codexDefaultContextWindow   int64 = 272_000
	codexGPT56ContextWindow     int64 = 372_000
	codexGPT56OneMContextWindow int64 = 1_000_000
	// codexGPT6ContextWindow is the documented window of the GPT-6 family
	// (Astra, Sol, Luna: 1,050,000 context, 922,000 max input, 128,000 max
	// output -- developers.openai.com/api/docs/models/gpt-6-*).
	codexGPT6ContextWindow int64 = 1_050_000
	codexGPT6MaxInput      int64 = 922_000
	codexDefaultMaxTokens  int64 = 128_000
	// glm53ContextWindow is the documented window of the GLM-5.3 family
	// (glm-5.3, glm-5.3-flash, glm-5.3-flashx).
	glm53ContextWindow    int64 = 1_000_000
	glm53DefaultMaxTokens int64 = 131_072
)

// factKind selects how a row treats an existing value.
type factKind uint8

const (
	// factFloor raises a smaller or unknown value to the documented one;
	// it never lowers. A lowered window breaks compaction more quietly
	// than a missing one.
	factFloor factKind = iota
	// factDefault fills in only an unknown (0) value.
	factDefault
)

// modelIDMatcher matches canonical model ids ("-wm" is trimmed first).
// Exactly one selector is used: predicate, exact ids, prefix (optionally
// narrowed to a suffix set), or match-all.
type modelIDMatcher struct {
	predicate func(id string) bool
	exact     []string
	prefix    string
	suffixes  []string
	matchAll  bool
}

func (m modelIDMatcher) matches(id string) bool {
	id = strings.TrimSuffix(id, "-wm")
	switch {
	case m.predicate != nil:
		return m.predicate(id)
	case len(m.exact) > 0:
		for _, candidate := range m.exact {
			if candidate == id {
				return true
			}
		}
		return false
	case m.prefix != "":
		if !strings.HasPrefix(id, m.prefix) {
			return false
		}
		if len(m.suffixes) == 0 {
			return true
		}
		for _, suffix := range m.suffixes {
			if strings.HasSuffix(id, suffix) {
				return true
			}
		}
		return false
	default:
		return m.matchAll
	}
}

// modelFactRule is one documented-facts row. The first matching row for a
// provider wins; the order below mirrors the branches of the former
// codexContextWindow function.
type modelFactRule struct {
	provider         string
	match            modelIDMatcher
	kind             factKind
	contextWindow    int64
	defaultMaxTokens int64
	// maxInputTokens is the vendor-documented hard input limit; 0 when the
	// source documents none. The agent never budgets input above it.
	maxInputTokens int64
	// ensurePresent, when set, is appended to the provider's model list by
	// ApplyModelFacts if no entry with its ID exists.
	ensurePresent *catwalk.Model
	// source documents where the numbers come from.
	source string
}

// GLM53ReasoningLevels is the GLM-5.3 family's effort vocabulary (verified
// against docs.z.ai/guides/llm/glm-5.3): the shared single slice used by the
// ensure-present template below and by the atom registry in internal/cmd.
var GLM53ReasoningLevels = []string{"low", "high", "max"}

// glm53Template is the ensure-present template for zai: the GLM-5.3 entry is
// appended when neither catwalk, the live catalog, nor the user's config
// lists it. The context numbers come from the row, not from here.
var glm53Template = catwalk.Model{
	ID:                     "glm-5.3",
	Name:                   "GLM-5.3",
	CanReason:              true,
	ReasoningLevels:        GLM53ReasoningLevels,
	DefaultReasoningEffort: "high",
}

// modelFactRules is the table itself. Rows 2 and 3 (the gpt-5.6 family) are
// retained for documentation; superseded ids are dropped at parse time.
var modelFactRules = [...]modelFactRule{
	{
		provider:         "openai-codex",
		match:            modelIDMatcher{predicate: codexGPT6Documented},
		kind:             factFloor,
		contextWindow:    codexGPT6ContextWindow,
		defaultMaxTokens: codexDefaultMaxTokens,
		maxInputTokens:   codexGPT6MaxInput,
		source:           "developers.openai.com/api/docs/models/gpt-6-*",
	},
	{
		provider:         "openai-codex",
		match:            modelIDMatcher{exact: []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"}},
		kind:             factFloor,
		contextWindow:    codexGPT56OneMContextWindow,
		defaultMaxTokens: codexDefaultMaxTokens,
		source:           "superseded family (hidden at parse time), kept for documentation",
	},
	{
		provider:         "openai-codex",
		match:            modelIDMatcher{exact: []string{"gpt-5.6"}},
		kind:             factDefault,
		contextWindow:    codexGPT56ContextWindow,
		defaultMaxTokens: codexDefaultMaxTokens,
		source:           "superseded family (hidden at parse time), kept for documentation",
	},
	{
		provider:         "openai-codex",
		match:            modelIDMatcher{prefix: "gpt-6"},
		kind:             factDefault,
		contextWindow:    codexGPT6ContextWindow,
		defaultMaxTokens: codexDefaultMaxTokens,
		source:           "developers.openai.com/api/docs/models/gpt-6-*",
	},
	{
		provider:         "openai-codex",
		match:            modelIDMatcher{prefix: "gpt-5.6-"},
		kind:             factDefault,
		contextWindow:    codexGPT56ContextWindow,
		defaultMaxTokens: codexDefaultMaxTokens,
		source:           "superseded family (hidden at parse time), kept for documentation",
	},
	{
		provider:         "openai-codex",
		match:            modelIDMatcher{matchAll: true},
		kind:             factDefault,
		contextWindow:    codexDefaultContextWindow,
		defaultMaxTokens: codexDefaultMaxTokens,
		source:           "ChatGPT Codex catalog default",
	},
	{
		provider:         "zai",
		match:            modelIDMatcher{prefix: "glm-5.3"},
		kind:             factFloor,
		contextWindow:    glm53ContextWindow,
		defaultMaxTokens: glm53DefaultMaxTokens,
		ensurePresent:    &glm53Template,
		source:           "glm-5.3: docs.z.ai/guides/llm/glm-5.3 (window copied from GLM-5.2); flash: docs.z.ai/guides/vlm/glm-5.3-flash; flashx: operator, not independently verified",
	},
}

// UserModelFacts reports which metadata fields the user configured for a
// model (ContextWindow > 0 / DefaultMaxTokens > 0 in rush.json). A user-set
// field is never touched by ApplyModelFacts, not even to raise it to the
// documented floor. A zero in the config is unknown, not a user value.
type UserModelFacts struct {
	ContextWindow    bool
	DefaultMaxTokens bool
}

// ApplyModelFacts overlays the documented facts table onto models, in place,
// and appends ensure-present templates for missing rows. It is the only
// place model metadata rules live; call it once, on the final provider list.
func ApplyModelFacts(provider string, models []catwalk.Model, userSet map[string]UserModelFacts) []catwalk.Model {
	var rules []*modelFactRule
	for i := range modelFactRules {
		rule := &modelFactRules[i]
		if rule.provider == provider {
			rules = append(rules, rule)
		}
	}
	for i := range models {
		model := &models[i]
		var rule *modelFactRule
		for _, candidate := range rules {
			if candidate.match.matches(model.ID) {
				rule = candidate
				break
			}
		}
		if rule == nil {
			continue
		}
		user := userSet[model.ID]
		if !user.ContextWindow {
			switch rule.kind {
			case factFloor:
				if model.ContextWindow < rule.contextWindow {
					model.ContextWindow = rule.contextWindow
				}
			case factDefault:
				if model.ContextWindow == 0 {
					model.ContextWindow = rule.contextWindow
				}
			}
		}
		if !user.DefaultMaxTokens && model.DefaultMaxTokens == 0 && rule.defaultMaxTokens > 0 {
			model.DefaultMaxTokens = rule.defaultMaxTokens
		}
		// The output budget never exceeds the window.
		if model.ContextWindow > 0 && model.DefaultMaxTokens > model.ContextWindow && !user.DefaultMaxTokens {
			model.DefaultMaxTokens = model.ContextWindow
		}
	}
	for _, rule := range rules {
		if rule.ensurePresent == nil {
			continue
		}
		missing := true
		for i := range models {
			if models[i].ID == rule.ensurePresent.ID {
				missing = false
				break
			}
		}
		if missing {
			entry := *rule.ensurePresent
			user := userSet[entry.ID]
			if !user.ContextWindow {
				entry.ContextWindow = rule.contextWindow
			}
			if !user.DefaultMaxTokens {
				entry.DefaultMaxTokens = rule.defaultMaxTokens
			}
			models = append(models, entry)
		}
	}
	return models
}

// LookupModelFacts returns the documented (context window, default max
// tokens) for provider/model, or (0, 0) when the table has no row for it.
func LookupModelFacts(provider, id string) (int64, int64) {
	for i := range modelFactRules {
		rule := &modelFactRules[i]
		if rule.provider != provider || !rule.match.matches(id) {
			continue
		}
		return rule.contextWindow, rule.defaultMaxTokens
	}
	return 0, 0
}

// LookupModelMaxInput returns the vendor-documented maximum input tokens for
// provider/model, or 0 when the table documents none (window minus output
// reserve is then the only bound).
func LookupModelMaxInput(provider, id string) int64 {
	for i := range modelFactRules {
		rule := &modelFactRules[i]
		if rule.provider != provider || !rule.match.matches(id) {
			continue
		}
		return rule.maxInputTokens
	}
	return 0
}

// The single model-visibility table: which model families are hidden from
// every list (CLI listings, the web picker, provider catalogs and their
// caches) because the provider superseded them. One predicate, one table —
// do not add per-surface filtering elsewhere.
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
