package agent

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/config"
)

func (c *coordinator) finishCustomRoleResolveSnapshot(ctx context.Context, cfg *config.Config, smartModel Model, selected config.SelectedModel, source string, wantTools bool) (*resolvedOverrides, error) {
	fastCfg := cfg.Models[config.SelectedModelTypeFast]
	if fastCfg.Provider == "" || fastCfg.Model == "" {
		fastCfg = selected
	}
	fastModel, _, err := c.buildModelsFromCfg(ctx, cfg, fastCfg, fastCfg, true)
	if err != nil {
		return nil, fmt.Errorf("failed to build companion fast model: %w", err)
	}
	providerCfg, ok := cfg.Providers.Get(selected.Provider)
	if !ok {
		return nil, fmt.Errorf("custom role provider %s not configured", selected.Provider)
	}
	if err := c.rejectScopedCallOnCLIProvider(ctx, "smart", providerCfg); err != nil {
		return nil, err
	}
	resolved := &resolvedOverrides{
		smart:        smartModel,
		fast:         fastModel,
		smartSource:  source,
		fastSource:   hbSourceSlotLive,
		providerCfg:  providerCfg,
		promptPrefix: providerCfg.SystemPromptPrefix,
	}
	resolved.persistSmartModel, resolved.persistFastModel = sessionModelPersistenceFrom(ctx)
	if c.prompt != nil {
		prompt, promptErr := c.prompt.Build(ctx, smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model, c.cfg, cfg, false)
		if promptErr != nil {
			slog.Error("resolveSessionModels: failed to build custom-role system prompt", "err", promptErr)
		} else {
			resolved.systemPrompt = prompt
		}
	}
	resolved.orchestrator = false
	if wantTools {
		if opts := callOptionsFrom(ctx); opts != nil {
			EnsureCallBuildScope(opts)
		}
		resolved.tools, err = c.pinCallTools(ctx, cfg)
		if err != nil {
			return nil, err
		}
	}
	return resolved, nil
}
