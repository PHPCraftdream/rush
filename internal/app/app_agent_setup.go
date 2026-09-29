// Coordinator wiring and model selection: InitCoderAgent builds the
// agent.Coordinator, and the model-override helpers resolve per-run
// smart/fast model choices for `rush run`.

package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/shell"
)

func (app *App) UpdateAgentModel(ctx context.Context) error {
	if app.AgentCoordinator == nil {
		return fmt.Errorf("agent configuration is missing")
	}
	return app.AgentCoordinator.UpdateModels(ctx)
}

// resolveModelOverridesForNonInteractive parses and validates model strings
// without publishing or changing any process-wide model state.
// Format: "model-name" (searches all providers) or "provider/model-name".
func (app *App) resolveModelOverridesForNonInteractive(smartModel, fastModel string) (*agent.ModelOverride, *agent.ModelOverride, error) {
	providers := app.config.Config().Providers.Copy()

	smartMatches, fastMatches, err := findModels(providers, smartModel, fastModel)
	if err != nil {
		return nil, nil, err
	}

	var smartOverride, fastOverride *agent.ModelOverride

	if smartModel != "" {
		found, err := validateMatches(smartMatches, smartModel, "smart")
		if err != nil {
			return nil, nil, err
		}
		slog.Info("Overriding smart model for non-interactive run", "provider", found.provider, "model", found.modelID)
		smartOverride = &agent.ModelOverride{
			Provider: found.provider,
			Model:    found.modelID,
		}
	}

	if fastModel != "" {
		found, err := validateMatches(fastMatches, fastModel, "fast")
		if err != nil {
			return nil, nil, err
		}
		slog.Info("Overriding fast model for non-interactive run", "provider", found.provider, "model", found.modelID)
		fastOverride = &agent.ModelOverride{
			Provider: found.provider,
			Model:    found.modelID,
		}
	}

	return smartOverride, fastOverride, nil
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
	if app.BackgroundShellManager == nil {
		app.BackgroundShellManager = shell.NewBackgroundShellManager()
	}
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
	// Phase-4 durable job store (docs/plans/2026-09-28-async-phase4-durable-
	// core.md sec.5 step 2/7): App.New already builds this unconditionally
	// (step 7: read-only status surfaces need it even when no provider is
	// configured and InitCoderAgent never runs) -- this is now just the
	// fallback for a test fixture that constructed *App by hand without
	// going through App.New. A pump-only App with no data dir (dataDir ==
	// "") still gets no store -- InitCoderAgent is never called on that
	// shape in production (it has no coder agent to init), but a test
	// fixture reaching this with dataDir == "" would get a coordinator that
	// fails closed on the first non-sync async tool call, not a crash.
	if app.asyncJobStore == nil && app.dataDir != "" {
		// label is display-only ("sessions jobs"/"sessions hosts", doc
		// sec.3.6) -- this App type drives both `rush run` and the web
		// server, and does not know which at construction time, so "app"
		// is used uniformly rather than guessing "cli"/"web" wrong.
		app.asyncJobStore = session.NewAsyncJobStore(app.DB(), app.dataDir, os.Getpid(), "app")
		// Doc sec.3.7: the first-registration dead-host sweep (ensureHost)
		// reads a recovered delegation's child text via this -- wired once,
		// before the first Claim can happen.
		app.asyncJobStore.SetMessages(app.Messages)
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
		app.mcpOwner,
		app.asyncJobStore,
		app.BackgroundShellManager,
	)
	if err != nil {
		slog.Error("Failed to create coder agent", "err", err)
		return err
	}
	return nil
}
