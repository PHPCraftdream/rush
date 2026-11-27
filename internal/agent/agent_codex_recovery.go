package agent

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

const codexRecoveryNudge = "Continue the original request using the conversation above. Do not repeat completed work."

type codexContextRejection interface {
	CodexContextRejectedBeforeOutput() bool
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
		Cfg: cfg, Session: sess, Creds: creds, UserID: call.ExistingMessageID,
		MaxTokens: call.MaxTokens, MaxCost: call.MaxCost, TargetMaxOutput: call.MaxOutputTokens,
	}
}

func (a *sessionAgent) recoveryWorker(ctx context.Context, state *codexRecoveryState) (*SummarizeSnapshot, error) {
	if state == nil || state.Cfg == nil || a.coord == nil || a.coord.cfg == nil {
		return nil, errors.New("automatic context recovery has no pinned worker configuration")
	}
	var model Model
	var providerCfg config.ProviderConfig
	if state.Creds != nil {
		choice, covered := state.Creds.Models[RoleWorker]
		if covered {
			var err error
			model, providerCfg, err = a.coord.buildCredentialModel(ctx, state.Cfg, state.Creds, choice)
			if err != nil {
				return nil, fmt.Errorf("pinned credential worker is unavailable: %w", err)
			}
		} else if !state.Creds.AllowConfiguredRoleFallback {
			return nil, errors.New("pinned credential set does not cover the worker role")
		} else {
			selected, _, err := requestedRoleSlot(state.Cfg, state.Session, RequestedModelRole(config.SelectedModelTypeWorker))
			if err != nil {
				return nil, fmt.Errorf("pinned worker role is unavailable: %w", err)
			}
			var ok bool
			providerCfg, ok = state.Cfg.Providers.Get(selected.Provider)
			if !ok {
				return nil, fmt.Errorf("pinned worker provider %q is unavailable", selected.Provider)
			}
			if providerCfg.Type == cliprovider.ProviderType {
				return nil, fmt.Errorf("automatic context recovery cannot use CLI worker provider %q", selected.Provider)
			}
			model, _, err = a.coord.buildModelsFromCfg(ctx, state.Cfg, selected, selected, false)
			if err != nil {
				return nil, fmt.Errorf("pinned worker model is unavailable: %w", err)
			}
		}
	} else {
		if a.coord.drainCredentialBoundary(ctx, state.Session.ID) {
			return nil, diskport.ErrCredentialBindingUnavailable
		}
		selected, _, err := requestedRoleSlot(state.Cfg, state.Session, RequestedModelRole(config.SelectedModelTypeWorker))
		if err != nil {
			return nil, fmt.Errorf("pinned worker role is unavailable: %w", err)
		}
		var ok bool
		providerCfg, ok = state.Cfg.Providers.Get(selected.Provider)
		if !ok {
			return nil, fmt.Errorf("pinned worker provider %q is unavailable", selected.Provider)
		}
		if providerCfg.Type == cliprovider.ProviderType {
			return nil, fmt.Errorf("automatic context recovery cannot use CLI worker provider %q", selected.Provider)
		}
		model, _, err = a.coord.buildModelsFromCfg(ctx, state.Cfg, selected, selected, false)
		if err != nil {
			return nil, fmt.Errorf("pinned worker model is unavailable: %w", err)
		}
	}
	if err := checkPeakHours(providerCfg); err != nil {
		return nil, err
	}
	return &SummarizeSnapshot{
		Model: model, ProviderOptions: modelkit.GetProviderOptions(state.Session.ID, model, providerCfg),
		PromptPrefix: providerCfg.SystemPromptPrefix, ProviderCfg: providerCfg,
	}, nil
}

func (a *sessionAgent) recoverCodexContext(ctx context.Context, call SessionAgentCall, model Model, prefix string, state *codexRecoveryState) (SessionAgentCall, bool, error) {
	if state == nil || state.Attempted.Swap(true) {
		return SessionAgentCall{}, false, errors.New("automatic context recovery budget exhausted")
	}
	if err := ctx.Err(); err != nil {
		return SessionAgentCall{}, false, err
	}
	if state.UserID == "" {
		return SessionAgentCall{}, false, errors.New("automatic context recovery cannot identify the original user message")
	}
	if state.TargetModel.Model == nil {
		state.TargetModel = model
	}
	if state.TargetPrefix == "" {
		state.TargetPrefix = prefix
	}
	if state.TargetModel.Model == nil || state.TargetModel.ModelCfg.Model == "" {
		return SessionAgentCall{}, false, errors.New("automatic context recovery has no captured target model")
	}
	worker, err := a.recoveryWorker(ctx, state)
	if err != nil {
		return SessionAgentCall{}, false, err
	}
	if worker.Model.CatwalkCfg.ContextWindow <= 0 {
		return SessionAgentCall{}, false, errors.New("automatic context recovery requires known worker context metadata")
	}
	workerMaxOutput := provauth.EffectiveMaxOutputTokens(worker.Model, 0)
	if workerMaxOutput <= 0 || workerMaxOutput >= int64(worker.Model.CatwalkCfg.ContextWindow) {
		return SessionAgentCall{}, false, errors.New("automatic context recovery requires known worker output reserve")
	}
	if state.MaxCost > 0 {
		budget, budgetErr := a.sessions.SubtreeBudget(ctx, call.SessionID)
		if budgetErr != nil {
			return SessionAgentCall{}, false, fmt.Errorf("automatic context recovery cannot read cost budget: %w", budgetErr)
		}
		if budget >= state.MaxCost {
			return SessionAgentCall{}, false, errors.New("automatic context recovery exhausted the call cost cap")
		}
	}
	if state.MaxTokens > 0 && state.MaxTokens-state.Spent.Load() <= 0 {
		return SessionAgentCall{}, false, errors.New("automatic context recovery exhausted the call token cap")
	}
	if err := ctx.Err(); err != nil {
		return SessionAgentCall{}, false, err
	}
	if err := a.runSummarizeSilentForRecovery(ctx, call.SessionID, worker.ProviderOptions, worker.Model, worker.PromptPrefix, state.UserID, workerMaxOutput, state); err != nil {
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
	continuation.Attachments = cloneRecoveryAttachments(state.TargetAttachments)
	continuation.Tools = append([]fantasy.AgentTool(nil), state.TargetTools...)
	continuation.SmartModel = &state.TargetModel
	pinnedPrefix := state.TargetPrefix
	continuation.SystemPromptPrefix = &pinnedPrefix
	continuation.ExistingMessageID = state.UserID
	continuation.MaxOutputTokens = state.TargetMaxOutput
	continuation.RecoveryContinuation = true
	continuation.CompactionContinuation = true
	continuation.Recovery = state
	continuation.RecoverySpentTokens = state.Spent.Load()
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
		state.Spent.Add(provauth.AddStepUsageTokens(0, usage))
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

// cleanupUncommittedSummary, recoveryInputCapacity and
// recoverySummarySystemMessages moved to provauth (recovery.go /
// summary_budget.go) as CleanupUncommittedSummary, RecoveryInputCapacity and
// RecoverySummarySystemMessages (DM3 chunk E).

func (a *sessionAgent) checkRecoverySummaryInput(ctx context.Context, sessionID string, summary message.Message, tail, pinned []message.Message, todos []session.Todo, state *codexRecoveryState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := a.sessions.Get(ctx, sessionID); err != nil {
		return errors.New("automatic context recovery cannot reload target session")
	}
	candidate := make([]message.Message, 0, 1+len(tail)+len(pinned))
	candidate = append(candidate, pinned...)
	visibleSummary := summary.Clone()
	visibleSummary.Role = message.User
	candidate = append(candidate, visibleSummary)
	candidate = append(candidate, tail...)
	prepared, files := a.preparePrompt(candidate, todos, state.TargetModel.Model.Provider(), state.TargetAttachments...)
	prepared = a.workaroundProviderMediaLimitations(prepared, state.TargetModel)
	prepared = append(prepared, fantasy.NewUserMessage(codexRecoveryNudge))
	prepared = append([]fantasy.Message{fantasy.NewSystemMessage(state.TargetSystem)}, prepared...)
	return provauth.RecoveryInputCapacity(state.TargetModel, state.TargetMaxOutput, state.TargetPrefix, prepared, files, state.TargetTools)
}

func (a *sessionAgent) accountRecoverySummary(ctx context.Context, sessionID, messageID string, model Model, usage fantasy.Usage, cost float64, snapshot session.Session) error {
	var errs []error
	if cost != 0 {
		if _, err := a.sessions.IncrementCost(ctx, sessionID, cost); err != nil {
			errs = append(errs, err)
		}
	}
	if snapshot.ID != "" {
		if err := a.sessions.SetUsage(ctx, sessionID, snapshot.PromptTokens, snapshot.CompletionTokens); err != nil {
			errs = append(errs, err)
		}
	}
	callbeat.AddHeartbeatUsage(ctx, model.Model.Provider(), model.Model.Model(), usage, cost)
	a.recordMessageUsage(ctx, messageID, model, usage, cost, false)
	return errors.Join(errs...)
}

func (a *sessionAgent) checkRecoverySummaryCost(ctx context.Context, sessionID string, maxCost, delta float64) error {
	if maxCost <= 0 {
		return nil
	}
	spent, err := a.sessions.SubtreeBudget(ctx, sessionID)
	if err != nil {
		return errors.New("automatic context recovery cannot verify the call cost cap")
	}
	if spent+delta > maxCost {
		return errors.New("automatic context recovery summary would exceed the call cost cap")
	}
	return nil
}
