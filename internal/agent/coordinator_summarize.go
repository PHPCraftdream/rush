// System prompt access, summarize snapshot and queue handling, and prompt
// queue introspection. Extracted from coordinator.go — pure code move,
// bodies unchanged.

package agent

import (
	"context"
	"fmt"
	"log/slog"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
)

// SummarizeSnapshot holds an immutable snapshot of all configuration needed
// for a single manual/queued summarize operation. It is computed ONCE from the
// target session's persisted models (or from shared state for sessions without
// overrides) and passed through the entire summarize path, ensuring the provider
// options, model, and prompt prefix never diverge due to concurrent SetModels
// calls (task #341, P1-1).
//
// This mirrors the resolvedOverrides pattern used for normal turns, but
// specialized for summarize which doesn't need fastModel or systemPrompt.
type SummarizeSnapshot struct {
	model           Model
	providerOptions fantasy.ProviderOptions
	promptPrefix    string
}

func (c *coordinator) GetSystemPrompt() string {
	return c.currentAgent.SystemPrompt()
}

func (c *coordinator) BuildSystemPrompt(ctx context.Context) (string, error) {
	if c.prompt == nil {
		return "", nil
	}

	// Build the default smart model from config for prompt building.
	cfg, _ := c.cfg.Snapshot()
	smartCfg := cfg.Models[config.SelectedModelTypeSmart]
	fastCfg := cfg.Models[config.SelectedModelTypeFast]

	smartModel, _, err := c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
	if err != nil {
		return "", fmt.Errorf("failed to build default smart model: %w", err)
	}

	// Use the same pinned cfg captured above (task #341, P1-1) instead of
	// re-reading c.cfg.Config() live inside workerSubAgentActive.
	return c.prompt.Build(ctx, smartModel.ModelCfg.Provider, smartModel.ModelCfg.Model, c.cfg, cfg, c.workerSubAgentActiveForCall(ctx, cfg))
}

// BuildSystemPromptForSession builds a system prompt for a specific session,
// using the session's model configuration (from DB overrides or config defaults).
// This ensures the system prompt matches the model that will actually run for this session.
func (c *coordinator) BuildSystemPromptForSession(ctx context.Context, sessionID string) (string, error) {
	if c.prompt == nil {
		return "", nil
	}

	// Resolve the session's model configuration (DB overrides → config defaults).
	resolved, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve session models: %w", err)
	}

	// Reuse the system prompt resolveSessionModels already built from its own
	// single pinned config snapshot (task #341, P1-1), rather than rebuilding
	// it here from a second, separately-timed live cfg read
	// (c.workerSubAgentActive() with no argument). A reload landing between
	// the two builds could otherwise make this second build's
	// WorkerAvailable flag disagree with resolved.smart, which was pinned
	// from an earlier generation.
	return resolved.systemPrompt, nil
}

func (c *coordinator) UpdateSessionSystemPrompt(ctx context.Context, sessionID, prompt string) error {
	return c.sessions.UpdateSystemPrompt(ctx, sessionID, prompt)
}

func (c *coordinator) QueuedPrompts(sessionID string) int {
	return c.currentAgent.QueuedPrompts(sessionID)
}

func (c *coordinator) QueuedPromptsList(sessionID string) []string {
	return c.currentAgent.QueuedPromptsList(sessionID)
}

// Summarize implements Coordinator.
func (c *coordinator) Summarize(ctx context.Context, sessionID string, snapshot *SummarizeSnapshot) error {
	// If the caller didn't provide a snapshot (e.g., tests), resolve one now.
	// This maintains backward compatibility while encouraging the correct pattern.
	if snapshot == nil {
		var err error
		snapshot, err = c.buildSummarizeSnapshot(ctx, sessionID)
		if err != nil {
			return err
		}
	}

	// Refresh OAuth token if needed using the provider from the snapshot.
	// Auth identity is already captured in the snapshot via providerCfg,
	// which is read once during snapshot construction, so no additional
	// pinning is needed here (task #341, P1-1).
	providerCfg, ok := c.cfg.Config().Providers.Get(snapshot.model.ModelCfg.Provider)
	if !ok {
		return errModelProviderNotConfigured
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return err
	}

	if err := c.refreshTokenIfExpired(ctx, providerCfg); err != nil {
		slog.Error("Failed to refresh OAuth2 token before summarize. Proceeding with existing token.", "error", err)
	}

	summarize := func() error {
		return c.currentAgent.Summarize(ctx, sessionID, snapshot)
	}

	// Summarize doesn't need a rebuild callback since it uses a pre-built
	// snapshot that doesn't capture a provider client.
	return c.runWithUnauthorizedRetry(ctx, providerCfg, summarize, nil)
}

// buildSummarizeSnapshot creates an immutable snapshot for a summarize operation,
// resolving the model from the target session's persisted models (or config defaults
// for sessions without overrides). This ensures the entire summarize operation
// uses consistent configuration regardless of concurrent session model changes.
func (c *coordinator) buildSummarizeSnapshot(ctx context.Context, sessionID string) (*SummarizeSnapshot, error) {
	// Resolve the session's model configuration from the DB or config defaults.
	// This always returns a valid snapshot (never nil).
	resolved, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve session models for summarize: %w", err)
	}

	// Get the provider config for this model.
	providerCfg, ok := c.cfg.Config().Providers.Get(resolved.smart.ModelCfg.Provider)
	if !ok {
		return nil, errModelProviderNotConfigured
	}

	// Build provider options from the resolved model.
	opts := getProviderOptions(sessionID, resolved.smart, providerCfg)

	// Use the prompt prefix from the resolved snapshot (provider config's
	// prefix, already set by resolveSessionModels).
	promptPrefix := resolved.promptPrefix
	if promptPrefix == "" {
		promptPrefix = providerCfg.SystemPromptPrefix
	}

	return &SummarizeSnapshot{
		model:           resolved.smart,
		providerOptions: opts,
		promptPrefix:    promptPrefix,
	}, nil
}

func (c *coordinator) SummarizeQueued(sessionID string) bool {
	return c.currentAgent.SummarizeQueued(sessionID)
}

func (c *coordinator) TakeSummarizeQueue(sessionID string) (*SummarizeSnapshot, bool) {
	return c.currentAgent.TakeSummarizeQueue(sessionID)
}

func (c *coordinator) CancelQueuedSummarize(sessionID string) {
	c.currentAgent.CancelQueuedSummarize(sessionID)
}
