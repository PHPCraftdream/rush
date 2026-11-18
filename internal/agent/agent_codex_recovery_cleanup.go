package agent

import (
	"context"
	"errors"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/callbeat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"

	"charm.land/fantasy"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

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
