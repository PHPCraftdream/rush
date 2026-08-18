// Provider and model resolution: merging catwalk's known providers with
// user config (plus local CLI providers and custom-provider model
// discovery), and computing the effective large/small model selection
// written back into cfg.Models.
package config

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/agent/cliprovider"
	"github.com/charmbracelet/crush/internal/agent/hyper"
	"github.com/charmbracelet/crush/internal/discover"
	"github.com/charmbracelet/crush/internal/env"
)

func (c *Config) configureProviders(ctx context.Context, store *ConfigStore, baseEnv env.Env, resolver VariableResolver, knownProviders []catwalk.Provider) error {
	knownProviderNames := make(map[string]bool)

	// Overlay CRUSH_X -> X explicitly for both env.Get lookups below and
	// the resolver, rather than mutating the process environment (see
	// crushEnvOverlay's doc comment for why). The overlay is scoped to
	// this call: no global state changes, so concurrent reloads/resolves
	// never see or clobber each other's overlay.
	overlay := crushEnvOverlay(baseEnv)
	env := env.NewOverlay(baseEnv, overlay)
	if len(overlay) > 0 {
		if r, ok := resolver.(*shellVariableResolver); ok {
			// Preserve the caller's expander override (tests) and bound
			// ctx (see resolve.go's WithContext) — only the env changes
			// here, not the resolver's other behaviour.
			resolver = NewShellVariableResolver(env, WithExpander(r.expand), WithContext(r.ctx))
		} else {
			resolver = NewShellVariableResolver(env, WithContext(ctx))
		}
	}

	// When disable_default_providers is enabled, skip all default/embedded
	// providers entirely. Users must fully specify any providers they want.
	// We skip to the custom provider validation loop which handles all
	// user-configured providers uniformly.
	if c.Options.DisableDefaultProviders {
		knownProviders = nil
	}

	for _, p := range knownProviders {
		knownProviderNames[string(p.ID)] = true
		config, configExists := c.Providers.Get(string(p.ID))
		// if the user configured a known provider we need to allow it to override a couple of parameters
		if configExists {
			if config.BaseURL != "" {
				p.APIEndpoint = config.BaseURL
			}
			if config.APIKey != "" {
				p.APIKey = config.APIKey
			}
			if len(config.Models) > 0 {
				models := []catwalk.Model{}
				seen := make(map[string]bool)

				for _, model := range config.Models {
					if seen[model.ID] {
						continue
					}
					seen[model.ID] = true
					if model.Name == "" {
						model.Name = model.ID
					}
					models = append(models, model)
				}
				for _, model := range p.Models {
					if seen[model.ID] {
						continue
					}
					seen[model.ID] = true
					if model.Name == "" {
						model.Name = model.ID
					}
					models = append(models, model)
				}

				p.Models = models
			}
		}

		headers := map[string]string{}
		if len(p.DefaultHeaders) > 0 {
			maps.Copy(headers, p.DefaultHeaders)
		}
		if len(config.ExtraHeaders) > 0 {
			maps.Copy(headers, config.ExtraHeaders)
		}
		// Provider headers use the same error contract as MCP headers:
		// a failing $(...) aborts the provider load with a clear
		// message, and a header that resolves to the empty string
		// (unset bare $VAR under lenient nounset, $(echo), or literal
		// "") is dropped from the outgoing request.
		for k, v := range headers {
			resolved, err := resolver.ResolveValue(v)
			if err != nil {
				return fmt.Errorf("resolving provider %s header %q: %w", p.ID, k, err)
			}
			if resolved == "" {
				delete(headers, k)
				continue
			}
			headers[k] = resolved
		}
		prepared := ProviderConfig{
			ID:                 string(p.ID),
			Name:               p.Name,
			BaseURL:            p.APIEndpoint,
			APIKey:             p.APIKey,
			APIKeyTemplate:     p.APIKey, // Store original template for re-resolution
			OAuthToken:         config.OAuthToken,
			Type:               p.Type,
			Disable:            config.Disable,
			SystemPromptPrefix: config.SystemPromptPrefix,
			ExtraHeaders:       headers,
			ExtraBody:          config.ExtraBody,
			ExtraParams:        make(map[string]string),
			Models:             p.Models,
			PeakHours:          config.PeakHours,
		}

		switch {
		case p.ID == catwalk.InferenceProviderAnthropic && config.OAuthToken != nil:
			// Claude Code subscription is not supported anymore. Remove to show onboarding.
			// removeConfigFieldBestEffort persists the deletion to disk under a
			// short, internal-only timeout (NOT RemoveConfigField's full 30s):
			// this call runs inside Load/reloadFromDiskLocked while publishMu is
			// held for the whole call, so a stall waiting on a contended or
			// wedged sibling process's sidecar lock would freeze the entire
			// config subsystem (including app startup) rather than just this
			// cleanup. See internalConfigWriteLockTimeout. Its auto-reload is a
			// no-op here regardless (the caller already holds publishMu). The
			// in-memory state stays consistent via Providers.Del below, and any
			// racing reload — or a future successful call to this same cleanup —
			// re-reads/retries the removal from disk.
			store.removeConfigFieldBestEffort(ScopeGlobal, "providers.anthropic")
			c.Providers.Del(string(p.ID))
			continue
		case p.ID == catwalk.InferenceProviderCopilot && config.OAuthToken != nil:
			prepared.SetupGitHubCopilot()
		}

		switch p.ID {
		// Handle specific providers that require additional configuration
		case catwalk.InferenceProviderVertexAI:
			var (
				project  = env.Get("VERTEXAI_PROJECT")
				location = env.Get("VERTEXAI_LOCATION")
			)
			if project == "" || location == "" {
				if configExists {
					slog.Warn("Skipping Vertex AI provider due to missing credentials")
					c.Providers.Del(string(p.ID))
				}
				continue
			}
			prepared.ExtraParams["project"] = project
			prepared.ExtraParams["location"] = location
		case catwalk.InferenceProviderAzure:
			endpoint, err := resolver.ResolveValue(p.APIEndpoint)
			if err != nil || endpoint == "" {
				if configExists {
					slog.Warn("Skipping Azure provider due to missing API endpoint", "provider", p.ID, "error", err)
					c.Providers.Del(string(p.ID))
				}
				continue
			}
			prepared.BaseURL = endpoint
			prepared.ExtraParams["apiVersion"] = env.Get("AZURE_OPENAI_API_VERSION")
		case catwalk.InferenceProviderBedrock:
			if p.APIKey == "" && !hasAWSCredentials(env) {
				if configExists {
					slog.Warn("Skipping Bedrock provider due to missing AWS credentials")
					c.Providers.Del(string(p.ID))
				}
				continue
			}
		case catwalk.InferenceProvider("hyper"):
			if apiKey := env.Get("HYPER_API_KEY"); apiKey != "" {
				prepared.APIKey = apiKey
				prepared.APIKeyTemplate = apiKey
			} else {
				v, err := resolver.ResolveValue(p.APIKey)
				if v == "" || err != nil {
					if configExists {
						slog.Warn("Skipping Hyper provider due to missing API key", "provider", p.ID)
						c.Providers.Del(string(p.ID))
					}
					continue
				}
			}
		case catwalk.InferenceProviderZAI:
			// Fork patch (orchestrator UX): ZAI_API_KEY (the primary) is
			// resolved through the configured template, which is either an
			// explicit providers.zai.api_key override or the embedded
			// "$ZAI_API_KEY" default. ZHIPU_API_KEY is accepted as a
			// fallback so users coming from Zhipu AI's own tooling, which
			// documents that variable name, don't need to set a second
			// variable. It's only consulted when the primary resolves
			// CLEANLY to empty, so ZAI_API_KEY always wins. Both names
			// honour the CRUSH_ prefix via the overlay built above.
			v, err := resolver.ResolveValue(p.APIKey)
			switch {
			case err != nil:
				// An explicitly configured api_key (e.g. a "$(...)" command)
				// that FAILED to resolve is a real misconfiguration. Warn and
				// skip like the default case rather than silently masking the
				// failure with the ZHIPU fallback and then sending requests
				// with the wrong/rotated key.
				if configExists {
					slog.Warn("Skipping Z.AI provider due to API key resolution error", "provider", p.ID, "error", err)
					c.Providers.Del(string(p.ID))
				}
				continue
			case v == "":
				// Primary resolved cleanly to empty (ZAI_API_KEY unset). Fall
				// back to ZHIPU_API_KEY if present, else warn+skip.
				if apiKey := env.Get("ZHIPU_API_KEY"); apiKey != "" {
					prepared.APIKey = apiKey
					prepared.APIKeyTemplate = apiKey
				} else {
					if configExists {
						slog.Warn("Skipping Z.AI provider due to missing API key", "provider", p.ID)
						c.Providers.Del(string(p.ID))
					}
					continue
				}
			}
			// v != "" && err == nil: the primary key resolved — keep the
			// template unchanged so ZAI_API_KEY wins.

			// Fork patch (orchestrator UX): synthesize a GLM-5.3 model
			// entry. Neither docs.z.ai nor the upstream catwalk provider
			// registry lists "glm-5.3" yet (as of 2026-08-14;
			// docs.z.ai/guides/llm/glm-5.3 404s), so prepared.Models
			// built from catwalk above never contains it — even though
			// the model itself is real and reachable, confirmed live via
			// `crush ping --model zai/glm-5.3` (see models_atoms.go's
			// glm5_3 atom entry for the full verification note and its
			// own "provisional numbers" caveat, which this entry mirrors
			// exactly for consistency between the two surfaces).
			//
			// Without this, `crush models use zai/glm-5.3` and `crush
			// ping --model zai/glm-5.3` already work (they resolve
			// provider/model directly, not through the catwalk catalog —
			// see ping.go's resolvePingModel), but the WEB UI's model
			// picker reads KnownProviders()/prepared.Models and would
			// never show glm-5.3 as a choice. Values (context window,
			// reasoning levels, default effort) are copied from zai's own
			// real glm-5.2 catwalk entry, not guessed — the two models
			// share the same 1M-context, high/xhigh-reasoning tier per
			// models_atoms.go's zaiReasoningLevels comment.
			//
			// Never overwrites a real catwalk-sourced or user-configured
			// entry: skipped entirely once catwalk (or the user's own
			// providers.zai.models config) actually lists glm-5.3.
			const glm53ModelID = "glm-5.3"
			hasGLM53 := false
			for _, m := range prepared.Models {
				if m.ID == glm53ModelID {
					hasGLM53 = true
					break
				}
			}
			if !hasGLM53 {
				extended := make([]catwalk.Model, len(prepared.Models), len(prepared.Models)+1)
				copy(extended, prepared.Models)
				prepared.Models = append(extended, catwalk.Model{
					ID:                     glm53ModelID,
					Name:                   "GLM-5.3",
					ContextWindow:          1_000_000,
					DefaultMaxTokens:       131072,
					CanReason:              true,
					ReasoningLevels:        []string{"high", "xhigh"},
					DefaultReasoningEffort: "xhigh",
				})
			}
		default:
			// if the provider api or endpoint are missing we skip them
			v, err := resolver.ResolveValue(p.APIKey)
			if v == "" || err != nil {
				if configExists {
					slog.Warn("Skipping provider due to missing API key", "provider", p.ID)
					c.Providers.Del(string(p.ID))
				}
				continue
			}
		}
		c.Providers.Set(string(p.ID), prepared)
	}

	// Add locally available CLI models (claude, gemini, etc.) as a built-in provider.
	// This runs after catwalk providers so CLI models appear alongside API models.
	if specs := cliprovider.Available(); len(specs) > 0 {
		models := make([]catwalk.Model, 0, len(specs))
		for _, spec := range specs {
			models = append(models, catwalk.Model{
				ID:               spec.ModelID,
				Name:             spec.ModelName,
				ContextWindow:    spec.ContextWindow,
				DefaultMaxTokens: 32768,
			})
		}
		// Start from whatever is already in c.Providers for this ID (loaded
		// from crush.json on this same pass) rather than a bare literal, so
		// user-set fields — peak_hours, disable, a custom display name,
		// system_prompt_prefix, etc. — survive being re-synthesized here on
		// every config load/reload. Only ID/Type/Models are ever
		// auto-derived; everything else is user-owned and must round-trip.
		// This block used to always overwrite with a fresh literal, which
		// silently discarded peak_hours (and any other custom field) for
		// this provider on every single load.
		provider, existed := c.Providers.Get(cliprovider.ProviderID)
		if !existed {
			provider = ProviderConfig{Name: "Local CLI"}
		}
		provider.ID = cliprovider.ProviderID
		provider.Type = cliprovider.ProviderType
		provider.Models = models
		c.Providers.Set(cliprovider.ProviderID, provider)
		knownProviderNames[cliprovider.ProviderID] = true // skip custom-provider validation
	}

	// Discover models concurrently for custom providers that need it.
	// A provider needs discovery when discover_models is explicitly true,
	// or when the models list is empty (auto-trigger, unless opted out).
	type discoveryResult struct {
		models []catwalk.Model
		err    error
	}

	discoveryResults := make(map[string]discoveryResult)
	var discoverMu sync.Mutex
	var discoverWg sync.WaitGroup

	discoverCtx, discoverCancel := context.WithTimeout(ctx, 3*time.Second)
	for id, pc := range c.Providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}
		if pc.Disable || pc.BaseURL == "" {
			continue
		}
		wantsDiscovery := pc.AutoDiscoverModels != nil && *pc.AutoDiscoverModels
		autoTrigger := len(pc.Models) == 0 && (pc.AutoDiscoverModels == nil || *pc.AutoDiscoverModels)
		if !wantsDiscovery && !autoTrigger {
			continue
		}
		providerID := cmp.Or(pc.ID, id)
		dcfg := discover.Config{
			ID:             providerID,
			BaseURL:        pc.BaseURL,
			APIKey:         pc.APIKey,
			ExtraHeaders:   pc.ExtraHeaders,
			ExistingModels: pc.Models,
		}
		providerType := cmp.Or(pc.Type, catwalk.TypeOpenAICompat)
		discoverWg.Go(func() {
			models, err := discover.DiscoverModels(discoverCtx, dcfg, resolver)
			if err == nil && len(models) > 0 {
				if enricher := discover.GetEnricher(string(providerType)); enricher != nil {
					models, _ = enricher.EnrichModels(discoverCtx, dcfg, resolver, models)
				}
			}
			discoverMu.Lock()
			discoveryResults[id] = discoveryResult{models: models, err: err}
			discoverMu.Unlock()
		})
	}
	discoverWg.Wait()
	discoverCancel()

	// validate the custom providers
	for id, providerConfig := range c.Providers.Seq2() {
		if knownProviderNames[id] {
			continue
		}

		// Make sure the provider ID is set
		providerConfig.ID = id
		providerConfig.Name = cmp.Or(providerConfig.Name, id) // Use ID as name if not set
		// default to OpenAI if not set
		providerConfig.Type = cmp.Or(providerConfig.Type, catwalk.TypeOpenAICompat)
		if !slices.Contains(catwalk.KnownProviderTypes(), providerConfig.Type) &&
			providerConfig.Type != hyper.Name &&
			!discover.IsKnownCustomProvider(string(providerConfig.Type)) {
			slog.Warn("Skipping custom provider due to unsupported provider type", "provider", id)
			c.Providers.Del(id)
			continue
		}

		if providerConfig.Disable {
			slog.Debug("Skipping custom provider due to disable flag", "provider", id)
			c.Providers.Del(id)
			continue
		}
		if providerConfig.APIKey == "" {
			slog.Warn("Provider is missing API key, this might be OK for local providers", "provider", id)
		}
		if providerConfig.BaseURL == "" {
			slog.Warn("Skipping custom provider due to missing API endpoint", "provider", id)
			c.Providers.Del(id)
			continue
		}

		// Apply discovery results if available.
		if result, ok := discoveryResults[id]; ok {
			if result.err != nil {
				slog.Warn("Model discovery failed", "provider", id, "error", result.err)
				if len(providerConfig.Models) == 0 {
					slog.Warn("Skipping provider with no models after failed discovery", "provider", id)
					c.Providers.Del(id)
					continue
				}
			} else if len(result.models) > 0 {
				providerConfig.Models = result.models
				slog.Info("Discovered models for provider", "provider", id, "count", len(result.models))
			}
		}

		if len(providerConfig.Models) == 0 {
			slog.Warn("Skipping custom provider because the provider has no models", "provider", id)
			c.Providers.Del(id)
			continue
		}
		apiKey, err := resolver.ResolveValue(providerConfig.APIKey)
		if apiKey == "" || err != nil {
			slog.Warn("Provider is missing API key, this might be OK for local providers", "provider", id)
		}
		baseURL, err := resolver.ResolveValue(providerConfig.BaseURL)
		if baseURL == "" || err != nil {
			slog.Warn("Skipping custom provider due to missing API endpoint", "provider", id, "error", err)
			c.Providers.Del(id)
			continue
		}

		// Custom-provider headers share the MCP error contract; see
		// the known-provider loop above.
		for k, v := range providerConfig.ExtraHeaders {
			resolved, err := resolver.ResolveValue(v)
			if err != nil {
				return fmt.Errorf("resolving provider %s header %q: %w", id, k, err)
			}
			if resolved == "" {
				delete(providerConfig.ExtraHeaders, k)
				continue
			}
			providerConfig.ExtraHeaders[k] = resolved
		}

		c.Providers.Set(id, providerConfig)
	}

	if c.Providers.Len() == 0 && c.Options.DisableDefaultProviders {
		return fmt.Errorf("default providers are disabled and there are no custom providers are configured")
	}

	// Validate peak_hours windows for every configured provider (known,
	// custom, and CLI). A malformed HH:MM string is a hard config-load
	// error, not a silent skip.
	for id, pc := range c.Providers.Seq2() {
		if pc.PeakHours == nil {
			continue
		}
		if err := pc.PeakHours.Validate(); err != nil {
			return fmt.Errorf("provider %s: %w", id, err)
		}
	}

	return nil
}

func (c *Config) defaultModelSelection(knownProviders []catwalk.Provider) (largeModel SelectedModel, smallModel SelectedModel, err error) {
	if len(knownProviders) == 0 && c.Providers.Len() == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return largeModel, smallModel, err
	}

	// Use the first provider enabled based on the known providers order
	// if no provider found that is known use the first provider configured
	for _, p := range knownProviders {
		providerConfig, ok := c.Providers.Get(string(p.ID))
		if !ok || providerConfig.Disable {
			continue
		}
		defaultLargeModel := c.GetModel(string(p.ID), p.DefaultLargeModelID)
		if defaultLargeModel == nil {
			slog.Warn("Default large model not found for provider, falling back to first available",
				"model", p.DefaultLargeModelID, "provider", p.ID)
			if len(providerConfig.Models) == 0 {
				return largeModel, smallModel, fmt.Errorf("default large model %s not found for provider %s", p.DefaultLargeModelID, p.ID)
			}
			defaultLargeModel = &providerConfig.Models[0]
		}
		largeModel = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultLargeModel.ID,
			MaxTokens:       defaultLargeModel.DefaultMaxTokens,
			ReasoningEffort: defaultLargeModel.DefaultReasoningEffort,
		}

		defaultSmallModel := c.GetModel(string(p.ID), p.DefaultSmallModelID)
		if defaultSmallModel == nil {
			slog.Warn("Default small model not found for provider, falling back to first available",
				"model", p.DefaultSmallModelID, "provider", p.ID)
			if len(providerConfig.Models) == 0 {
				return largeModel, smallModel, fmt.Errorf("default small model %s not found for provider %s", p.DefaultSmallModelID, p.ID)
			}
			defaultSmallModel = &providerConfig.Models[0]
		}
		smallModel = SelectedModel{
			Provider:        string(p.ID),
			Model:           defaultSmallModel.ID,
			MaxTokens:       defaultSmallModel.DefaultMaxTokens,
			ReasoningEffort: defaultSmallModel.DefaultReasoningEffort,
		}
		return largeModel, smallModel, err
	}

	enabledProviders := c.EnabledProviders()
	slices.SortFunc(enabledProviders, func(a, b ProviderConfig) int {
		return strings.Compare(a.ID, b.ID)
	})

	if len(enabledProviders) == 0 {
		err = fmt.Errorf("no providers configured, please configure at least one provider")
		return largeModel, smallModel, err
	}

	providerConfig := enabledProviders[0]
	if len(providerConfig.Models) == 0 {
		err = fmt.Errorf("provider %s has no models configured", providerConfig.ID)
		return largeModel, smallModel, err
	}
	defaultLargeModel := c.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	largeModel = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultLargeModel.ID,
		MaxTokens: defaultLargeModel.DefaultMaxTokens,
	}
	defaultSmallModel := c.GetModel(providerConfig.ID, providerConfig.Models[0].ID)
	smallModel = SelectedModel{
		Provider:  providerConfig.ID,
		Model:     defaultSmallModel.ID,
		MaxTokens: defaultSmallModel.DefaultMaxTokens,
	}
	return largeModel, smallModel, err
}

// configureSelectedModels computes the effective large/small model
// selection for cfg and writes the result back into cfg.Models. store is
// only consulted when persist is true (the initial Load path), to persist
// a corrected selection back to disk via UpdatePreferredModel — it is
// never read for its own Config(), so callers preparing a *Config that has
// not yet been published to the store (e.g. reloadFromDiskLocked building
// the next generation locally) can pass any store here; only its
// persistence side effects (disk write + eventual reload) are used.
func configureSelectedModels(store *ConfigStore, cfg *Config, knownProviders []catwalk.Provider, persist bool) error {
	c := cfg
	defaultLarge, defaultSmall, err := c.defaultModelSelection(knownProviders)
	if err != nil {
		return fmt.Errorf("failed to select default models: %w", err)
	}
	large, small := defaultLarge, defaultSmall

	largeModelSelected, largeModelConfigured := c.Models[SelectedModelTypeLarge]
	if largeModelConfigured {
		if largeModelSelected.Model != "" {
			large.Model = largeModelSelected.Model
		}
		if largeModelSelected.Provider != "" {
			large.Provider = largeModelSelected.Provider
		}
		model := c.GetModel(large.Provider, large.Model)
		if model == nil {
			large = defaultLarge
			if persist {
				// Use the Locked variant because Load (the only caller with
				// persist=true) already holds publishMu. The public
				// UpdatePreferredModel would deadlock re-acquiring it.
				if err := store.updatePreferredModelLocked(ScopeGlobal, SelectedModelTypeLarge, large); err != nil {
					return fmt.Errorf("failed to update preferred large model: %w", err)
				}
			}
		} else {
			if largeModelSelected.MaxTokens > 0 {
				large.MaxTokens = largeModelSelected.MaxTokens
			} else {
				large.MaxTokens = model.DefaultMaxTokens
			}
			if largeModelSelected.ReasoningEffort != "" {
				large.ReasoningEffort = largeModelSelected.ReasoningEffort
			} else {
				large.ReasoningEffort = model.DefaultReasoningEffort
			}
			large.Think = largeModelSelected.Think
			if largeModelSelected.Temperature != nil {
				large.Temperature = largeModelSelected.Temperature
			}
			if largeModelSelected.TopP != nil {
				large.TopP = largeModelSelected.TopP
			}
			if largeModelSelected.TopK != nil {
				large.TopK = largeModelSelected.TopK
			}
			if largeModelSelected.FrequencyPenalty != nil {
				large.FrequencyPenalty = largeModelSelected.FrequencyPenalty
			}
			if largeModelSelected.PresencePenalty != nil {
				large.PresencePenalty = largeModelSelected.PresencePenalty
			}
		}
	}
	smallModelSelected, smallModelConfigured := c.Models[SelectedModelTypeSmall]
	if smallModelConfigured {
		if smallModelSelected.Model != "" {
			small.Model = smallModelSelected.Model
		}
		if smallModelSelected.Provider != "" {
			small.Provider = smallModelSelected.Provider
		}

		model := c.GetModel(small.Provider, small.Model)
		if model == nil {
			small = defaultSmall
			if persist {
				if err := store.updatePreferredModelLocked(ScopeGlobal, SelectedModelTypeSmall, small); err != nil {
					return fmt.Errorf("failed to update preferred small model: %w", err)
				}
			}
		} else {
			if smallModelSelected.MaxTokens > 0 {
				small.MaxTokens = smallModelSelected.MaxTokens
			} else {
				small.MaxTokens = model.DefaultMaxTokens
			}
			if smallModelSelected.ReasoningEffort != "" {
				small.ReasoningEffort = smallModelSelected.ReasoningEffort
			} else {
				small.ReasoningEffort = model.DefaultReasoningEffort
			}
			if smallModelSelected.Temperature != nil {
				small.Temperature = smallModelSelected.Temperature
			}
			if smallModelSelected.TopP != nil {
				small.TopP = smallModelSelected.TopP
			}
			if smallModelSelected.TopK != nil {
				small.TopK = smallModelSelected.TopK
			}
			if smallModelSelected.FrequencyPenalty != nil {
				small.FrequencyPenalty = smallModelSelected.FrequencyPenalty
			}
			if smallModelSelected.PresencePenalty != nil {
				small.PresencePenalty = smallModelSelected.PresencePenalty
			}
			small.Think = smallModelSelected.Think
		}
	}

	// When small isn't explicitly configured and the provider isn't a
	// known built-in, use the large model as the small model. This
	// prevents two different models from being requested concurrently
	// for local/openai-compat providers.
	if !smallModelConfigured {
		isKnownProvider := false
		for _, kp := range knownProviders {
			if string(kp.ID) == small.Provider {
				isKnownProvider = true
				break
			}
		}
		if !isKnownProvider {
			slog.Warn("Using large model as small model for unknown provider", "provider", large.Provider, "model", large.Model)
			small = large
		}
	}

	c.Models[SelectedModelTypeLarge] = large
	c.Models[SelectedModelTypeSmall] = small
	return nil
}
