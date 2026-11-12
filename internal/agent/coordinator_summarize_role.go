package agent

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

type StandaloneSummarizer interface {
	SummarizeWithRole(context.Context, string, config.SelectedModelType) error
}

type standaloneSummarizer struct {
	cfg      *config.ConfigStore
	sessions session.Service
	messages message.Service
}

func NewStandaloneSummarizer(cfg *config.ConfigStore, sessions session.Service, messages message.Service) StandaloneSummarizer {
	return &standaloneSummarizer{cfg: cfg, sessions: sessions, messages: messages}
}

func SummaryRoleAllowed(role config.SelectedModelType) bool {
	switch role {
	case config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
		return true
	default:
		return false
	}
}

func (s *standaloneSummarizer) SummarizeWithRole(ctx context.Context, sessionID string, role config.SelectedModelType) error {
	if !SummaryRoleAllowed(role) {
		return fmt.Errorf("unsupported standalone summary role %q", role)
	}
	sess, err := s.sessions.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to load session %q: %w", sessionID, err)
	}
	cfg, _ := s.cfg.Snapshot()
	selected, _, err := requestedRoleSlot(cfg, sess, RequestedModelRole(role))
	if err != nil {
		return fmt.Errorf("%s model is unavailable: %w", role, err)
	}
	providerCfg, ok := cfg.Providers.Get(selected.Provider)
	if !ok {
		return fmt.Errorf("%s provider %q is not configured", role, selected.Provider)
	}
	if providerCfg.Type == cliprovider.ProviderType {
		return fmt.Errorf("standalone compaction does not support CLI provider %q: CLI tool authority cannot be safely restricted on this path", selected.Provider)
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return err
	}
	dataDir := ""
	if cfg.Options != nil {
		dataDir = strings.TrimSpace(cfg.Options.DataDirectory)
	}
	if dataDir == "" {
		return fmt.Errorf("standalone compaction requires a configured data directory for session locking")
	}
	coord := &coordinator{cfg: s.cfg, sessions: s.sessions, messages: s.messages}
	model, _, err := coord.buildModelsFromCfg(ctx, cfg, selected, selected, false)
	if err != nil {
		return fmt.Errorf("%s model is unavailable: %w", role, err)
	}
	agent := NewSessionAgent(SessionAgentOptions{
		SmartModel:           model,
		FastModel:            model,
		DisableAutoSummarize: true,
		Sessions:             s.sessions,
		Messages:             s.messages,
		Config:               s.cfg,
		DataDirectory:        dataDir,
		Tools:                []fantasy.AgentTool{},
	})
	snapshot := &SummarizeSnapshot{
		model:           model,
		providerOptions: getProviderOptions(sessionID, model, providerCfg),
		promptPrefix:    providerCfg.SystemPromptPrefix,
		providerCfg:     providerCfg,
	}
	// Preserve the selected worker's session identity and configured effort for
	// direct Codex requests (cache affinity and reasoning dispatch).
	summaryCtx := ctx
	if providerCfg.ID == "openai-codex" {
		summaryCtx = context.WithValue(summaryCtx, cliprovider.SessionIDContextKey, sessionID)
		summaryCtx = context.WithValue(summaryCtx, cliprovider.ReasoningEffortContextKey, turnSmartReasoningEffort(model))
	}
	return agent.(*sessionAgent).SummarizeNow(summaryCtx, sessionID, snapshot)
}
