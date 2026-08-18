// Model-slot preferences and related copy-on-write mutators: per-scope
// reads of the models.* slots, persisted and runtime-only preferred-model
// writes, the agent allowed-tools runtime override, and recent-model
// bookkeeping.
package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
)

// ReadModelsAtScope reads the per-scope `models.smart` / `models.fast` entries
// directly from the on-disk file for the given scope, ignoring any merge with
// the other scope. Returns (nil, nil) for a slot that the scope's file does not
// define; returns an error only on read/parse failure. Used by `crush models
// state` to show "what each scope says" alongside the effective merged view.
//
// Fork patch: batch 11 — `crush models state` needs per-scope visibility.
func (s *ConfigStore) ReadModelsAtScope(scope Scope) (smart, fast *SelectedModel, err error) {
	all, err := s.ReadAllModelsAtScope(scope)
	if err != nil {
		return nil, nil, err
	}
	return all[SelectedModelTypeSmart], all[SelectedModelTypeFast], nil
}

// ReadAllModelsAtScope reads the per-scope `models.*` entries for all four
// slots (large, small, worker, reviewer) directly from the on-disk file for
// the given scope, ignoring any merge with the other scope. Missing slots are
// absent from the returned map; returns an error only on read/parse failure.
//
// Fork patch: worker/reviewer CLI settability — `crush models state` needs
// per-scope visibility into all four slots, not just smart/fast.
func (s *ConfigStore) ReadAllModelsAtScope(scope Scope) (map[SelectedModelType]*SelectedModel, error) {
	path, perr := s.configPath(scope)
	if perr != nil {
		// No path for this scope (e.g. workspace not initialised) — treat as
		// "nothing set". Not an error.
		return map[SelectedModelType]*SelectedModel{}, nil
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return map[SelectedModelType]*SelectedModel{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, rerr)
	}
	var sm struct {
		Models map[SelectedModelType]SelectedModel `json:"models"`
	}
	if err := json.Unmarshal(data, &sm); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(map[SelectedModelType]*SelectedModel, len(sm.Models))
	for _, slot := range []SelectedModelType{SelectedModelTypeSmart, SelectedModelTypeFast, SelectedModelTypeWorker, SelectedModelTypeReviewer} {
		if v, ok := sm.Models[slot]; ok {
			v := v
			out[slot] = &v
		}
	}
	return out, nil
}

// UpdatePreferredModel updates the preferred model for the given type and
// persists it to the config file at the given scope.
func (s *ConfigStore) UpdatePreferredModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	return s.UpdatePreferredModels(scope, map[SelectedModelType]SelectedModel{modelType: model})
}

// UpdatePreferredModels updates and persists multiple model slots (e.g.
// smart/fast/worker/reviewer) in a single write via SetConfigFields, so
// callers that need to set several slots at once (like `crush models use`)
// get one atomic on-disk write instead of one write per slot. Callers are
// responsible for validating every entry in models BEFORE calling this —
// this function assumes all inputs are already valid and only performs
// writes; it does not partially apply on error, but nor does it need to,
// since validation is expected to have already happened.
func (s *ConfigStore) UpdatePreferredModels(scope Scope, models map[SelectedModelType]SelectedModel) error {
	if len(models) == 0 {
		return nil
	}
	fields := make(map[string]any, len(models))
	for modelType, model := range models {
		fields[fmt.Sprintf("models.%s", modelType)] = model
	}
	if err := s.SetConfigFields(scope, fields); err != nil {
		return fmt.Errorf("failed to update preferred models: %w", err)
	}
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.Models = maps.Clone(cfgCopy.Models)
		if cfgCopy.Models == nil {
			cfgCopy.Models = make(map[SelectedModelType]SelectedModel, len(models))
		}
		for modelType, model := range models {
			cfgCopy.Models[modelType] = model
		}
	})
	for modelType, model := range models {
		if err := s.recordRecentModel(scope, modelType, model); err != nil {
			return err
		}
	}
	return nil
}

// updatePreferredModelLocked is the re-entrant-safe variant of
// UpdatePreferredModel for callers that already hold publishMu (i.e. Load
// via configureSelectedModels). It uses updateConfigLocked /
// recordRecentModelLocked instead of the lock-taking variants.
func (s *ConfigStore) updatePreferredModelLocked(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	return s.updatePreferredModelsLocked(scope, map[SelectedModelType]SelectedModel{modelType: model})
}

// updatePreferredModelsLocked is the re-entrant-safe variant of
// UpdatePreferredModels for callers that already hold publishMu.
func (s *ConfigStore) updatePreferredModelsLocked(scope Scope, models map[SelectedModelType]SelectedModel) error {
	if len(models) == 0 {
		return nil
	}
	fields := make(map[string]any, len(models))
	for modelType, model := range models {
		fields[fmt.Sprintf("models.%s", modelType)] = model
	}
	if err := s.SetConfigFields(scope, fields); err != nil {
		return fmt.Errorf("failed to update preferred models: %w", err)
	}
	s.updateConfigLocked(func(cfgCopy *Config) {
		cfgCopy.Models = maps.Clone(cfgCopy.Models)
		if cfgCopy.Models == nil {
			cfgCopy.Models = make(map[SelectedModelType]SelectedModel, len(models))
		}
		for modelType, model := range models {
			cfgCopy.Models[modelType] = model
		}
	})
	for modelType, model := range models {
		if err := s.recordRecentModelLocked(scope, modelType, model); err != nil {
			return err
		}
	}
	return nil
}

// SetSelectedModelRuntime overrides a single model slot (smart/fast/
// worker/reviewer) in memory ONLY — no disk write, no autoReload, no
// recent-models bookkeeping. It exists for callers that need a
// process-lifetime override rather than a persisted preference, e.g.
// `crush run --model=...`/--fast-model=...`, which temporarily swaps the
// active model for one non-interactive invocation and must NOT leave that
// override sitting in crush.json for the next run to inherit.
//
// Before this method existed, that one-shot CLI override went through
// app.config.Config().Models[...] = ... — mutating the map returned by
// Config() directly from a different package, bypassing ConfigStore
// entirely and racing any concurrent reader of the same map. Now it goes
// through the same copy-on-write path (updateConfig) as every other
// mutator, just without the SetConfigFields disk round-trip.
func (s *ConfigStore) SetSelectedModelRuntime(modelType SelectedModelType, model SelectedModel) {
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.Models = maps.Clone(cfgCopy.Models)
		if cfgCopy.Models == nil {
			cfgCopy.Models = make(map[SelectedModelType]SelectedModel, 1)
		}
		cfgCopy.Models[modelType] = model
	})
}

// UpdateAgentAllowedTools replaces the given agent's AllowedTools in memory
// ONLY (not persisted to disk), via the same copy-on-write publish path as
// every other in-memory-only mutator (see SetSelectedModelRuntime,
// SetProviderRuntimeConfig). The change is published as a new generation
// instead of mutating whatever *Config a concurrent reader currently holds
// via Config() in place.
//
// It exists for callers like app.disableToolsInConfig (`crush run`'s
// sub-agent ban / smart+worker bypass), which used to do
// cfg := app.config.Config(); cfg.Agents[id] = agent — writing straight into
// the map backing the currently-published snapshot. Config() is documented
// as read-only after load; any reader that captured a *Config pointer before
// that write (e.g. a concurrent goroutine mid-turn) would see the mutated
// AllowedTools retroactively, and any reader that captures one after would
// see it too even though no new generation was actually published. Routing
// through updateConfig instead clones Agents (a map, so updateConfig's
// shallow top-level *Config copy alone does not protect it — mutate is
// responsible for cloning nested maps, see updateConfig's doc comment)
// before writing the single entry, so old snapshots are left untouched.
func (s *ConfigStore) UpdateAgentAllowedTools(agentID string, tools []string) {
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.Agents = maps.Clone(cfgCopy.Agents)
		agent := cfgCopy.Agents[agentID]
		agent.AllowedTools = tools
		cfgCopy.Agents[agentID] = agent
	})
}

// recordRecentModel records a model in the recent models list.
func (s *ConfigStore) recordRecentModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	if model.Provider == "" || model.Model == "" {
		return nil
	}

	eq := func(a, b SelectedModel) bool {
		return a.Provider == b.Provider && a.Model == b.Model
	}

	entry := SelectedModel{
		Provider: model.Provider,
		Model:    model.Model,
	}

	current := s.loadSnapshot().config.RecentModels[modelType]
	withoutCurrent := slices.DeleteFunc(slices.Clone(current), func(existing SelectedModel) bool {
		return eq(existing, entry)
	})

	updated := append([]SelectedModel{entry}, withoutCurrent...)
	if len(updated) > maxRecentModelsPerType {
		updated = updated[:maxRecentModelsPerType]
	}

	if slices.EqualFunc(current, updated, eq) {
		return nil
	}

	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.RecentModels = maps.Clone(cfgCopy.RecentModels)
		if cfgCopy.RecentModels == nil {
			cfgCopy.RecentModels = make(map[SelectedModelType][]SelectedModel)
		}
		cfgCopy.RecentModels[modelType] = updated
	})

	if err := s.SetConfigField(scope, fmt.Sprintf("recent_models.%s", modelType), updated); err != nil {
		return fmt.Errorf("failed to persist recent models: %w", err)
	}

	return nil
}

// recordRecentModelLocked is the re-entrant-safe variant of
// recordRecentModel for callers that already hold publishMu. It uses
// updateConfigLocked instead of updateConfig.
func (s *ConfigStore) recordRecentModelLocked(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	if model.Provider == "" || model.Model == "" {
		return nil
	}

	eq := func(a, b SelectedModel) bool {
		return a.Provider == b.Provider && a.Model == b.Model
	}

	entry := SelectedModel{
		Provider: model.Provider,
		Model:    model.Model,
	}

	current := s.loadSnapshot().config.RecentModels[modelType]
	withoutCurrent := slices.DeleteFunc(slices.Clone(current), func(existing SelectedModel) bool {
		return eq(existing, entry)
	})

	updated := append([]SelectedModel{entry}, withoutCurrent...)
	if len(updated) > maxRecentModelsPerType {
		updated = updated[:maxRecentModelsPerType]
	}

	if slices.EqualFunc(current, updated, eq) {
		return nil
	}

	s.updateConfigLocked(func(cfgCopy *Config) {
		cfgCopy.RecentModels = maps.Clone(cfgCopy.RecentModels)
		if cfgCopy.RecentModels == nil {
			cfgCopy.RecentModels = make(map[SelectedModelType][]SelectedModel)
		}
		cfgCopy.RecentModels[modelType] = updated
	})

	if err := s.SetConfigField(scope, fmt.Sprintf("recent_models.%s", modelType), updated); err != nil {
		return fmt.Errorf("failed to persist recent models: %w", err)
	}

	return nil
}

// RecordRecentModel records the given model as recently used and persists to disk.
func (s *ConfigStore) RecordRecentModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	return s.recordRecentModel(scope, modelType, model)
}

// RemoveRecentModel removes a model from the recent list and persists to disk.
func (s *ConfigStore) RemoveRecentModel(scope Scope, modelType SelectedModelType, model SelectedModel) error {
	current := s.loadSnapshot().config.RecentModels[modelType]
	if current == nil {
		return nil
	}
	updated := slices.DeleteFunc(slices.Clone(current), func(m SelectedModel) bool {
		return m.Provider == model.Provider && m.Model == model.Model
	})
	if len(updated) == len(current) {
		return nil
	}
	s.updateConfig(func(cfgCopy *Config) {
		cfgCopy.RecentModels = maps.Clone(cfgCopy.RecentModels)
		cfgCopy.RecentModels[modelType] = updated
	})
	if err := s.SetConfigField(scope, fmt.Sprintf("recent_models.%s", modelType), updated); err != nil {
		return fmt.Errorf("failed to persist recent models: %w", err)
	}
	return nil
}
