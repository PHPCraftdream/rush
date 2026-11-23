package config

import (
	"cmp"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
)

func defaultModelSelection(cfg *Config, knownProviders []catwalk.Provider) (smartModel SelectedModel, fastModel SelectedModel, err error) {
	if len(knownProviders) == 0 && cfg.Providers.Len() == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return smartModel, fastModel, err
	}

	// Use the first provider enabled based on the known providers order
	// if no provider found that is known use the first provider configured
	for _, p := range knownProviders {
		providerConfig, ok := cfg.Providers.Get(string(p.ID))
		if !ok || providerConfig.Disable {
			continue
		}
		if len(providerConfig.Models) == 0 && p.ID == catwalk.InferenceProvider("openai-codex") && providerConfig.OAuthToken != nil {
			continue
		}
		defaultSmartModel := cfg.GetModel(string(p.ID), p.DefaultLargeModelID)
		if defaultSmartModel == nil {
			if p.ID != catwalk.InferenceProvider("openai-codex") {
				slog.Warn("Default smart model not found for provider, falling back to first available",
					"model", p.DefaultLargeModelID, "provider", p.ID)
			}
			if len(providerConfig.Models) == 0 {
				return smartModel, fastModel, fmt.Errorf("default smart model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			}
			defaultSmartModel = &providerConfig.Models[0]
		}
		smartModel = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultSmartModel.ID,
			MaxTokens:       defaultSmartModel.DefaultMaxTokens,
			ReasoningEffort: defaultSmartModel.DefaultReasoningEffort,
		}

		defaultFastModel := cfg.GetModel(string(p.ID), p.DefaultSmallModelID)
		if defaultFastModel == nil {
			if p.ID != catwalk.InferenceProvider("openai-codex") {
				slog.Warn("Default fast model not found for provider, falling back to first available",
					"model", p.DefaultSmallModelID, "provider", p.ID)
			}
			if len(providerConfig.Models) == 0 {
				return smartModel, fastModel, fmt.Errorf("default fast model %s not found for provider %s", p.DefaultSmallModelID, p.ID)
			}
			defaultFastModel = &providerConfig.Models[0]
		}
		fastModel = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultFastModel.ID,
			MaxTokens:       defaultFastModel.DefaultMaxTokens,
			ReasoningEffort: defaultFastModel.DefaultReasoningEffort,
		}
		return smartModel, fastModel, err
	}

	enabledProviders := cfg.EnabledProviders()
	slices.SortFunc(enabledProviders, func(a, b ProviderConfig) int {
		return strings.Compare(a.ID, b.ID)
	})

	if len(enabledProviders) == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return smartModel, fastModel, err
	}

	providerConfig := enabledProviders[0]
	for _, candidate := range enabledProviders {
		if len(candidate.Models) > 0 {
			providerConfig = candidate
			break
		}
	}
	// Do not invent a default model when an authenticated account's catalog
	// is unavailable; keep the provider configurable for a later retry.
	if len(providerConfig.Models) == 0 {
		for _, candidate := range enabledProviders {
			if candidate.ID == "openai-codex" && candidate.OAuthToken != nil {
				return smartModel, fastModel, nil
			}
		}
		err = fmt.Errorf("provider %s has no models configured", providerConfig.ID)
		return smartModel, fastModel, err
	}
	defaultSmartModel := cfg.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	smartModel = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultSmartModel.ID,
		MaxTokens: defaultSmartModel.DefaultMaxTokens,
	}
	defaultFastModel := cfg.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	fastModel = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultFastModel.ID,
		MaxTokens: defaultFastModel.DefaultMaxTokens,
	}
	return smartModel, fastModel, err
}

// unverifiedPassthroughModel returns minimal stand-in metadata for a model
// that isn't in its provider's cached catalog but whose provider IS a real,
// enabled entry in this config — or nil when the provider is unknown,
// disabled, or the selection is missing a field, any of which still
// warrants falling back to a default (see the empty-field/Disable guards
// below; callers additionally only invoke this when both fields were
// explicitly set in the raw selection — see configureSelectedModels).
//
// This closes the load-side half of the "accept an unverified provider/model"
// behavior added on 2026-08-26. `rush models use zai/<brand-new-model>`
// (internal/app/provider.go's findModels/ResolveModel) deliberately accepts
// and persists such a selection with a warning, but configureSelectedModels
// used to overwrite it on the very next config load: any command that loads
// config — including read-only ones like `rush models state` — replaced the
// user's choice with the provider's DefaultLargeModelID AND persisted that
// replacement to the GLOBAL scope. A model can be live at the provider well
// before it appears in catwalk or a locally cached catalog, so "not in the
// catalog" is not evidence the selection is wrong.
//
// Every zero-value field here means "unknown, don't assume" and is already
// treated that way downstream (e.g. agent_turn.go gates context-window
// budgeting behind `if cw > 0`), matching the same synthesized-model approach
// coordinator_models.go's buildModelsFromCfg uses for this exact case.
//
// Callers must only invoke this when the CALLER'S OWN configured selection
// (not a value inherited from defaultModelSelection's merge) explicitly
// named both provider and model — configureSelectedModels' two call sites
// gate on smartModelSelected/fastModelSelected's raw fields for exactly this
// reason. Without that gate, a partial hand-edited selection (e.g. only
// `{"provider": "zai"}`, model left empty) would pair the chosen provider
// with whatever model defaultModelSelection happened to pick for a
// DIFFERENT provider, silently keeping a nonsensical combination alive
// instead of self-healing to a working default the way it used to.
func unverifiedPassthroughModel(c *Config, sel SelectedModel) *catwalk.Model {
	if sel.Provider == "" || sel.Model == "" {
		return nil
	}
	p, ok := c.Providers.Get(sel.Provider)
	if !ok || p.Disable {
		return nil
	}
	slog.Debug("Using unverified model selection not found in provider's cached catalog",
		"provider", sel.Provider, "model", sel.Model)
	return &catwalk.Model{ID: sel.Model, Name: sel.Model}
}

// ConfigureSelectedModels computes the effective smart/fast model
// selection for cfg and writes the result back into cfg.Models. An
// unresolvable selection falls back to the default in memory only.
func ConfigureSelectedModels(cfg *Config, knownProviders []catwalk.Provider) error {
	defaultSmart, defaultFast, err := defaultModelSelection(cfg, knownProviders)
	if err != nil {
		return fmt.Errorf("failed to select default models: %w", err)
	}
	smart, fast := defaultSmart, defaultFast

	smartModelSelected, smartModelConfigured := cfg.Models[SelectedModelTypeSmart]
	if smartModelConfigured {
		if smartModelSelected.Model != "" {
			smart.Model = smartModelSelected.Model
		}
		if smartModelSelected.Provider != "" {
			smart.Provider = smartModelSelected.Provider
		}
		model := cfg.GetModel(smart.Provider, smart.Model)
		if model == nil && smartModelSelected.Provider != "" && smartModelSelected.Model != "" {
			model = unverifiedPassthroughModel(cfg, smart)
		}
		if model == nil {
			smart = defaultSmart
			slog.Warn("Configured model not found in catalog; using default in memory only", "slot", SelectedModelTypeSmart, "selected", smartModelSelected.Provider+"/"+smartModelSelected.Model, "default", smart.Provider+"/"+smart.Model)
		} else {
			if smartModelSelected.MaxTokens > 0 {
				smart.MaxTokens = smartModelSelected.MaxTokens
			} else {
				smart.MaxTokens = model.DefaultMaxTokens
			}
			smart.ReasoningEffort = cmp.Or(smartModelSelected.ReasoningEffort, model.DefaultReasoningEffort)
			smart.ServiceTier = smartModelSelected.ServiceTier
			smart.Think = smartModelSelected.Think
			if smartModelSelected.Temperature != nil {
				smart.Temperature = smartModelSelected.Temperature
			}
			if smartModelSelected.TopP != nil {
				smart.TopP = smartModelSelected.TopP
			}
			if smartModelSelected.TopK != nil {
				smart.TopK = smartModelSelected.TopK
			}
			if smartModelSelected.FrequencyPenalty != nil {
				smart.FrequencyPenalty = smartModelSelected.FrequencyPenalty
			}
			if smartModelSelected.PresencePenalty != nil {
				smart.PresencePenalty = smartModelSelected.PresencePenalty
			}
		}
	}
	fastModelSelected, fastModelConfigured := cfg.Models[SelectedModelTypeFast]
	if fastModelConfigured {
		if fastModelSelected.Model != "" {
			fast.Model = fastModelSelected.Model
		}
		if fastModelSelected.Provider != "" {
			fast.Provider = fastModelSelected.Provider
		}

		model := cfg.GetModel(fast.Provider, fast.Model)
		if model == nil && fastModelSelected.Provider != "" && fastModelSelected.Model != "" {
			model = unverifiedPassthroughModel(cfg, fast)
		}
		if model == nil {
			fast = defaultFast
			slog.Warn("Configured model not found in catalog; using default in memory only", "slot", SelectedModelTypeFast, "selected", fastModelSelected.Provider+"/"+fastModelSelected.Model, "default", fast.Provider+"/"+fast.Model)
		} else {
			if fastModelSelected.MaxTokens > 0 {
				fast.MaxTokens = fastModelSelected.MaxTokens
			} else {
				fast.MaxTokens = model.DefaultMaxTokens
			}
			fast.ReasoningEffort = cmp.Or(fastModelSelected.ReasoningEffort, model.DefaultReasoningEffort)
			fast.ServiceTier = fastModelSelected.ServiceTier
			if fastModelSelected.Temperature != nil {
				fast.Temperature = fastModelSelected.Temperature
			}
			if fastModelSelected.TopP != nil {
				fast.TopP = fastModelSelected.TopP
			}
			if fastModelSelected.TopK != nil {
				fast.TopK = fastModelSelected.TopK
			}
			if fastModelSelected.FrequencyPenalty != nil {
				fast.FrequencyPenalty = fastModelSelected.FrequencyPenalty
			}
			if fastModelSelected.PresencePenalty != nil {
				fast.PresencePenalty = fastModelSelected.PresencePenalty
			}
			fast.Think = fastModelSelected.Think
		}
	}

	// When no fast default exists, reuse smart. Unknown providers also keep
	// the existing single-model fallback to avoid concurrent disparate models.
	if !fastModelConfigured && fast.Provider == "" {
		fast = smart
	} else if !fastModelConfigured {
		isKnownProvider := false
		for _, kp := range knownProviders {
			if string(kp.ID) == fast.Provider {
				isKnownProvider = true
				break
			}
		}
		if !isKnownProvider {
			slog.Warn("Using smart model as fast model for unknown provider", "provider", smart.Provider, "model", smart.Model)
			fast = smart
		}
	}

	cfg.Models[SelectedModelTypeSmart] = smart
	cfg.Models[SelectedModelTypeFast] = fast
	return nil
}
