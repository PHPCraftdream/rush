// Model-list merge helpers for provider configuration: reconciling
// configured, catalog and live-discovered model rows.
package config

import (
	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/discover"
)

// forgetDumpedUserSet clears the user-set flags of config rows whose values
// equal the catalog's raw numbers -- a `rush providers update` dump of a
// catalog that is only fetched later (the Codex account catalog, inside the
// provider loop) must not block the facts overlay. Rows absent from the
// catalog keep their flags, and the entry vanishes once nothing is set.
func forgetDumpedUserSet(userSet map[string]map[string]discover.UserModelFacts, providerID string, configModels, catalog []catwalk.Model) {
	if len(catalog) == 0 {
		return
	}
	byID := make(map[string]catwalk.Model, len(catalog))
	for _, model := range catalog {
		byID[model.ID] = model
	}
	set := userSet[providerID]
	for _, model := range configModels {
		entry, tracked := set[model.ID]
		if !tracked {
			continue
		}
		base, inCatalog := byID[model.ID]
		if !inCatalog {
			continue
		}
		if model.ContextWindow == base.ContextWindow {
			entry.ContextWindow = false
		}
		if model.DefaultMaxTokens == base.DefaultMaxTokens {
			entry.DefaultMaxTokens = false
		}
		if !entry.ContextWindow && !entry.DefaultMaxTokens {
			delete(set, model.ID)
			continue
		}
		set[model.ID] = entry
	}
}

func mergeCodexModels(configured, discovered []catwalk.Model) []catwalk.Model {
	models := make([]catwalk.Model, 0, len(configured)+len(discovered))
	seen := make(map[string]struct{}, len(configured)+len(discovered))
	for _, model := range configured {
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
	}
	for _, model := range discovered {
		if _, exists := seen[model.ID]; exists {
			continue
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
	}
	return models
}

func liveEffortsFromModels(models []catwalk.Model) map[string]ModelEffortInfo {
	var efforts map[string]ModelEffortInfo
	for i := range models {
		if len(models[i].ReasoningLevels) == 0 {
			continue
		}
		if efforts == nil {
			efforts = make(map[string]ModelEffortInfo)
		}
		efforts[models[i].ID] = ModelEffortInfo{
			Levels: models[i].ReasoningLevels, Default: models[i].DefaultReasoningEffort,
		}
	}
	return efforts
}

func mergeProviderModels(configured, live []catwalk.Model) []catwalk.Model {
	result := make([]catwalk.Model, len(configured), len(configured)+len(live))
	copy(result, configured)
	indices := make(map[string]int, len(configured)+len(live))
	for i := range result {
		indices[result[i].ID] = i
	}
	for i := range live {
		model := &live[i]
		if index, exists := indices[model.ID]; exists {
			if model.ContextWindow > 0 {
				result[index].ContextWindow = model.ContextWindow
			}
			if model.Name != model.ID {
				result[index].Name = model.Name
			}
			if len(model.ReasoningLevels) > 0 {
				result[index].CanReason = true
				result[index].ReasoningLevels = model.ReasoningLevels
				result[index].DefaultReasoningEffort = model.DefaultReasoningEffort
			}
			continue
		}
		indices[model.ID] = len(result)
		result = append(result, *model)
	}
	return result
}
