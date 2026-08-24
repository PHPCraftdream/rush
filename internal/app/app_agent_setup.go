// Coordinator wiring and model selection: InitCoderAgent builds the
// agent.Coordinator, and the model-override helpers resolve per-run
// smart/fast model choices for `rush run`.

package app

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
)

func (app *App) UpdateAgentModel(ctx context.Context) error {
	if app.AgentCoordinator == nil {
		return fmt.Errorf("agent configuration is missing")
	}
	return app.AgentCoordinator.UpdateModels(ctx)
}

// overrideModelsForNonInteractive parses the model strings and temporarily
// overrides the model configurations, then rebuilds the agent.
// Format: "model-name" (searches all providers) or "provider/model-name".
// Model matching is case-insensitive.
// If smartModel is provided but fastModel is not, the fast model defaults to
// the provider's default fast model.
func (app *App) overrideModelsForNonInteractive(ctx context.Context, smartModel, fastModel string) error {
	providers := app.config.Config().Providers.Copy()

	smartMatches, fastMatches, err := findModels(providers, smartModel, fastModel)
	if err != nil {
		return err
	}

	var smartProviderID string

	// Override smart model.
	if smartModel != "" {
		found, err := validateMatches(smartMatches, smartModel, "smart")
		if err != nil {
			return err
		}
		smartProviderID = found.provider
		slog.Info("Overriding smart model for non-interactive run", "provider", found.provider, "model", found.modelID)
		app.config.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		})
	}

	// Override fast model.
	switch {
	case fastModel != "":
		found, err := validateMatches(fastMatches, fastModel, "fast")
		if err != nil {
			return err
		}
		slog.Info("Overriding fast model for non-interactive run", "provider", found.provider, "model", found.modelID)
		app.config.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
			Provider: found.provider,
			Model:    found.modelID,
		})

	case smartModel != "":
		// No fast model specified, but smart model was - use provider's default.
		fastCfg := app.GetDefaultFastModel(smartProviderID)
		app.config.SetSelectedModelRuntime(config.SelectedModelTypeFast, fastCfg)
	}

	return app.AgentCoordinator.UpdateModels(ctx)
}

// GetDefaultFastModel returns the default fast model for the given
// provider. Falls back to the smart model if no default is found.
func (app *App) GetDefaultFastModel(providerID string) config.SelectedModel {
	cfg := app.config.Config()
	smartModelCfg := cfg.Models[config.SelectedModelTypeSmart]

	// Find the provider in the known providers list to get its default fast model.
	knownProviders, _ := config.Providers(cfg)
	var knownProvider *catwalk.Provider
	for _, p := range knownProviders {
		if string(p.ID) == providerID {
			knownProvider = &p
			break
		}
	}

	// For unknown/local providers, use the smart model as small.
	if knownProvider == nil {
		slog.Warn("Using smart model as fast model for unknown provider", "provider", providerID, "model", smartModelCfg.Model)
		return smartModelCfg
	}

	defaultFastModelID := knownProvider.DefaultSmallModelID
	model := cfg.GetModel(providerID, defaultFastModelID)
	if model == nil {
		slog.Warn("Default fast model not found, using smart model", "provider", providerID, "model", smartModelCfg.Model)
		return smartModelCfg
	}

	slog.Info("Using provider default fast model", "provider", providerID, "model", defaultFastModelID)
	return config.SelectedModel{
		Provider:        providerID,
		Model:           defaultFastModelID,
		MaxTokens:       model.DefaultMaxTokens,
		ReasoningEffort: model.DefaultReasoningEffort,
	}
}

// Fork merge note (origin/main 6716ef09 "feat(skills): user invocable skills"):
// upstream added setupEvents/setupSubscriber to forward service events into a
// bubbletea pubsub broker. We rejected this — our WebSocket hub
// (internal/server/hub.go) handles event fan-out to browser clients directly
// without going through tea.Msg. See CHANGELOG.fork.md Section 2.
func (app *App) InitCoderAgent(ctx context.Context) error {
	coderAgentCfg := app.config.Config().Agents[config.AgentCoder]
	if coderAgentCfg.ID == "" {
		// Self-heal: config.Load/reload always call SetupAgents once
		// IsConfigured() becomes true, but a caller that mutates
		// Providers/SelectedModel directly on an already-published config
		// (bypassing Load/reload entirely — a test-only pattern; found via
		// a CI-only failure this exact class of gap caused, "coder agent
		// configuration is missing", that never reproduced on a dev machine
		// with some stray provider config making IsConfigured() true at
		// initial Init) never triggers that population. SetupAgents is
		// idempotent (derives Agents purely from Options/DisabledTools, no
		// I/O), so re-deriving it here on a genuine miss is safe and closes
		// the whole class of gap at the one place every caller (test or
		// production) funnels through, rather than requiring every caller
		// to remember an explicit SetupAgents call of their own.
		app.config.SetupAgents()
		coderAgentCfg = app.config.Config().Agents[config.AgentCoder]
	}
	if coderAgentCfg.ID == "" {
		return fmt.Errorf("coder agent configuration is missing")
	}
	var err error
	app.AgentCoordinator, err = agent.NewCoordinator(
		ctx,
		app.config,
		app.Sessions,
		app.Messages,
		app.Permissions,
		app.History,
		app.FileTracker,
		app.agentNotifications,
	)
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}
	return nil
}
