package config

import "charm.land/catwalk/pkg/catwalk"

var supplementalCatalogModels = map[catwalk.InferenceProvider][]catwalk.Model{
	catwalk.InferenceProviderAnthropic: {
		{
			ID:                     "claude-haiku-5-5",
			Name:                   "Claude Haiku 5.5",
			ContextWindow:          1_000_000,
			DefaultMaxTokens:       64_000,
			CanReason:              true,
			ReasoningLevels:        []string{"low", "medium", "high", "xhigh", "max"},
			DefaultReasoningEffort: "high",
			SupportsImages:         true,
			// Anthropic list price for prompts up to 100K tokens; cache
			// write/read use the standard 1.25x/0.1x multipliers.
			CostPer1MIn:        0.10,
			CostPer1MOut:       0.50,
			CostPer1MInCached:  0.125,
			CostPer1MOutCached: 0.01,
		},
	},
}

func mergeSupplementalCatalogModels(providers []catwalk.Provider) []catwalk.Provider {
	for i := range providers {
		supplements := supplementalCatalogModels[providers[i].ID]
		if len(supplements) == 0 {
			continue
		}
		ids := make(map[string]struct{}, len(providers[i].Models)+len(supplements))
		for _, model := range providers[i].Models {
			ids[model.ID] = struct{}{}
		}
		for _, model := range supplements {
			if _, exists := ids[model.ID]; exists {
				continue
			}
			providers[i].Models = append(providers[i].Models, model)
			ids[model.ID] = struct{}{}
		}
	}
	return providers
}
