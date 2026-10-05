// Package-level single source of documented model metadata: the context
// window and default max output of the model families whose values are
// documented by their vendor (or asserted by the operator). Every other copy
// of these numbers was removed; ApplyModelFacts is the only function that
// applies them, and the final pass at the end of config's loadProviders is
// the only call site. Nothing may change ContextWindow or DefaultMaxTokens
// after that pass.
package discover

import (
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
