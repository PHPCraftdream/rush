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
		modelsByID := make(map[string]catwalk.Model, len(providers[i].Models))
		for _, model := range providers[i].Models {
			ids[model.ID] = struct{}{}
			modelsByID[model.ID] = model
		}
		for _, model := range supplements {
			if _, exists := ids[model.ID]; exists {
				continue
			}
			if model.ID == "claude-haiku-5-5" {
				if predecessor, exists := modelsByID["claude-haiku-4-5-20251001"]; exists {
					// estimate: pricing not published in catwalk; replaced as soon as catwalk lists the model
					model.CostPer1MIn = predecessor.CostPer1MIn
					model.CostPer1MOut = predecessor.CostPer1MOut
					model.CostPer1MInCached = predecessor.CostPer1MInCached
					model.CostPer1MOutCached = predecessor.CostPer1MOutCached
				}
			}
			providers[i].Models = append(providers[i].Models, model)
			ids[model.ID] = struct{}{}
		}
	}
	return providers
}
