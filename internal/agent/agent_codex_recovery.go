package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

const codexRecoveryNudge = "Continue the original request using the conversation above. Do not repeat completed work."

type codexContextRejection interface {
	CodexContextRejectedBeforeOutput() bool
}

type codexRecoveryState struct {
	cfg               *config.Config
	session           session.Session
	creds             *CredentialSet
	userID            string
	maxTokens         int64
	maxCost           float64
	targetModel       Model
	targetPrefix      string
	targetSystem      string
	targetPrompt      string
	targetMaxOutput   int64
	targetAttachments []message.Attachment
	targetTools       []fantasy.AgentTool
	spent             atomic.Int64
	attempted         atomic.Bool
}

func (s *codexRecoveryState) markUser(id string) {
	if s != nil && s.userID == "" {
		s.userID = id
	}
}

func codexContextRejectedBeforeOutput(err error) bool {
	if err == nil {
		return false
	}
	var markerFound, noReplay bool
	pending := []error{err}
	seen := make(map[error]struct{})
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 128 {
			return false
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		if comparableError(current) {
			if _, ok := seen[current]; ok {
				return false
			}
			seen[current] = struct{}{}
		}
		if marker, ok := current.(codexContextRejection); ok && marker.CodexContextRejectedBeforeOutput() {
			markerFound = true
		}
		if marker, ok := current.(interface{ NoReplay() bool }); ok && marker.NoReplay() {
			noReplay = true
		}
		if _, ok := current.(interface{ NoReplay() }); ok {
			noReplay = true
		}
		if multi, ok := current.(interface{ Unwrap() []error }); ok {
			children := multi.Unwrap()
			if len(pending)+len(children) > 128-visited-1 {
				return false
			}
			pending = append(pending, children...)
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			if len(pending) >= 128-visited-1 {
				return false
			}
			pending = append(pending, wrapped.Unwrap())
		}
	}
	return markerFound && !noReplay
}

func comparableError(err error) (comparable bool) {
	defer func() {
		if recover() != nil {
			comparable = false
		}
	}()
	_ = map[error]struct{}{err: {}}
	return true
}

func (a *sessionAgent) snapshotCodexRecovery(call SessionAgentCall, sess session.Session) *codexRecoveryState {
	if a.coord == nil || a.coord.cfg == nil {
		return nil
	}
	cfg, _ := a.coord.cfg.Snapshot()
	creds := call.Credentials
	if creds != nil {
		copy := creds.Clone()
		creds = &copy
	}
	return &codexRecoveryState{
		cfg: cfg, session: sess, creds: creds, userID: call.ExistingMessageID,
		maxTokens: call.MaxTokens, maxCost: call.MaxCost, targetMaxOutput: call.MaxOutputTokens,
	}
}

func (a *sessionAgent) recoveryWorker(ctx context.Context, state *codexRecoveryState) (*SummarizeSnapshot, error) {
	if state == nil || state.cfg == nil || a.coord == nil || a.coord.cfg == nil {
		return nil, errors.New("automatic context recovery has no pinned worker configuration")
	}
	var model Model
	var providerCfg config.ProviderConfig
	if state.creds != nil {
		choice, covered := state.creds.Models[RoleWorker]
		if covered {
			var err error
			model, providerCfg, err = a.coord.buildCredentialModel(ctx, state.cfg, state.creds, choice)
			if err != nil {
				return nil, fmt.Errorf("pinned credential worker is unavailable: %w", err)
			}
		} else if !state.creds.AllowConfiguredRoleFallback {
			return nil, errors.New("pinned credential set does not cover the worker role")
		} else {
			selected, _, err := requestedRoleSlot(state.cfg, state.session, RequestedModelRole(config.SelectedModelTypeWorker))
			if err != nil {
				return nil, fmt.Errorf("pinned worker role is unavailable: %w", err)
			}
			var ok bool
			providerCfg, ok = state.cfg.Providers.Get(selected.Provider)
			if !ok {
				return nil, fmt.Errorf("pinned worker provider %q is unavailable", selected.Provider)
			}
			if providerCfg.Type == cliprovider.ProviderType {
				return nil, fmt.Errorf("automatic context recovery cannot use CLI worker provider %q", selected.Provider)
			}
			model, _, err = a.coord.buildModelsFromCfg(ctx, state.cfg, selected, selected, false)
			if err != nil {
				return nil, fmt.Errorf("pinned worker model is unavailable: %w", err)
			}
		}
	} else {
		if a.coord.drainCredentialBoundary(ctx, state.session.ID) {
			return nil, ErrCredentialBindingUnavailable
		}
		selected, _, err := requestedRoleSlot(state.cfg, state.session, RequestedModelRole(config.SelectedModelTypeWorker))
		if err != nil {
			return nil, fmt.Errorf("pinned worker role is unavailable: %w", err)
		}
		var ok bool
		providerCfg, ok = state.cfg.Providers.Get(selected.Provider)
		if !ok {
			return nil, fmt.Errorf("pinned worker provider %q is unavailable", selected.Provider)
		}
		if providerCfg.Type == cliprovider.ProviderType {
			return nil, fmt.Errorf("automatic context recovery cannot use CLI worker provider %q", selected.Provider)
		}
		model, _, err = a.coord.buildModelsFromCfg(ctx, state.cfg, selected, selected, false)
		if err != nil {
			return nil, fmt.Errorf("pinned worker model is unavailable: %w", err)
		}
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return nil, err
	}
	return &SummarizeSnapshot{
		model: model, providerOptions: getProviderOptions(state.session.ID, model, providerCfg),
		promptPrefix: providerCfg.SystemPromptPrefix, providerCfg: providerCfg,
	}, nil
}

func (a *sessionAgent) recoverCodexContext(ctx context.Context, call SessionAgentCall, model Model, prefix string, state *codexRecoveryState) (SessionAgentCall, bool, error) {
	if state == nil || state.attempted.Swap(true) {
		return SessionAgentCall{}, false, errors.New("automatic context recovery budget exhausted")
	}
	if err := ctx.Err(); err != nil {
		return SessionAgentCall{}, false, err
	}
	if state.userID == "" {
		return SessionAgentCall{}, false, errors.New("automatic context recovery cannot identify the original user message")
	}
	if state.targetModel.Model == nil {
		state.targetModel = model
	}
	if state.targetPrefix == "" {
		state.targetPrefix = prefix
	}
	if state.targetModel.Model == nil || state.targetModel.ModelCfg.Model == "" {
		return SessionAgentCall{}, false, errors.New("automatic context recovery has no captured target model")
	}
	worker, err := a.recoveryWorker(ctx, state)
	if err != nil {
		return SessionAgentCall{}, false, err
	}
	if worker.model.CatwalkCfg.ContextWindow <= 0 {
		return SessionAgentCall{}, false, errors.New("automatic context recovery requires known worker context metadata")
	}
	workerMaxOutput := effectiveMaxOutputTokens(worker.model, 0)
	if workerMaxOutput <= 0 || workerMaxOutput >= int64(worker.model.CatwalkCfg.ContextWindow) {
		return SessionAgentCall{}, false, errors.New("automatic context recovery requires known worker output reserve")
	}
	if state.maxCost > 0 {
		budget, budgetErr := a.sessions.SubtreeBudget(ctx, call.SessionID)
		if budgetErr != nil {
			return SessionAgentCall{}, false, fmt.Errorf("automatic context recovery cannot read cost budget: %w", budgetErr)
		}
		if budget >= state.maxCost {
			return SessionAgentCall{}, false, errors.New("automatic context recovery exhausted the call cost cap")
		}
	}
	if state.maxTokens > 0 && state.maxTokens-state.spent.Load() <= 0 {
		return SessionAgentCall{}, false, errors.New("automatic context recovery exhausted the call token cap")
	}
	if err := ctx.Err(); err != nil {
		return SessionAgentCall{}, false, err
	}
	if err := a.runSummarizeSilentForRecovery(ctx, call.SessionID, worker.providerOptions, worker.model, worker.promptPrefix, state.userID, workerMaxOutput, state); err != nil {
		return SessionAgentCall{}, false, fmt.Errorf("automatic context recovery summary failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return SessionAgentCall{}, false, err
	}
	updated, err := a.sessions.Get(ctx, call.SessionID)
	if err != nil {
		return SessionAgentCall{}, false, fmt.Errorf("reload session after automatic summary: %w", err)
	}
	persisted, err := a.getSessionMessages(ctx, updated)
	if err != nil {
		return SessionAgentCall{}, false, fmt.Errorf("reload history after automatic summary: %w", err)
	}
	if updated.SummaryMessageID == "" || len(persisted) == 0 {
		return SessionAgentCall{}, false, errors.New("automatic context recovery did not persist a usable summary")
	}
	continuation := call
	continuation.Prompt = codexRecoveryNudge
	continuation.Attachments = cloneRecoveryAttachments(state.targetAttachments)
	continuation.Tools = append([]fantasy.AgentTool(nil), state.targetTools...)
	continuation.SmartModel = &state.targetModel
	pinnedPrefix := state.targetPrefix
	continuation.SystemPromptPrefix = &pinnedPrefix
	continuation.ExistingMessageID = state.userID
	continuation.MaxOutputTokens = state.targetMaxOutput
	continuation.recoveryContinuation = true
	continuation.compactionContinuation = true
	continuation.recovery = state
	continuation.recoverySpentTokens = state.spent.Load()
	return continuation, true, nil
}

func cloneRecoveryAttachments(attachments []message.Attachment) []message.Attachment {
	if attachments == nil {
		return nil
	}
	cloned := make([]message.Attachment, len(attachments))
	for i, attachment := range attachments {
		cloned[i] = attachment
		cloned[i].Content = append([]byte(nil), attachment.Content...)
	}
	return cloned
}

func addRecoveryUsage(state *codexRecoveryState, usage fantasy.Usage) {
	if state != nil {
		state.spent.Add(addStepUsageTokens(0, usage))
	}
}

func retainHistoryBoundary(msgs []message.Message, userID string) (int, bool) {
	for i := range msgs {
		if msgs[i].ID == userID {
			return i, true
		}
	}
	return 0, false
}
