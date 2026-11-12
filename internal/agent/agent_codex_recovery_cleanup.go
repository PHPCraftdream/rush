package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

func cleanupUncommittedSummary(messages message.Service, ctx context.Context, id string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), summaryCommitMaxDuration)
	defer cancel()
	return errors.Join(cause, messages.Delete(cleanupCtx, id))
}

// recoveryInputCapacity uses serialized UTF-8 bytes as a conservative upper
// bound, including files and every tool's advertised name/schema/description.
func recoveryInputCapacity(model Model, outputOverride int64, prefix string, messages []fantasy.Message, files []fantasy.FilePart, tools []fantasy.AgentTool) error {
	if model.Model == nil || int64(model.CatwalkCfg.ContextWindow) <= 0 || effectiveMaxOutputTokens(model, outputOverride) <= 0 {
		return errors.New("automatic context recovery requires known context and output limits")
	}
	inputBudget := modelInputBudget(model, outputOverride)
	if inputBudget <= 0 {
		return errors.New("automatic context recovery requires known context and output limits")
	}
	used, err := preparedInputTokenCount(messages, 0, prefix)
	if err != nil {
		return errors.New("automatic context recovery cannot safely estimate prepared text input")
	}
	add := func(size int) bool {
		if size < 0 || used > math.MaxInt64-int64(size) {
			return false
		}
		used += int64(size)
		return true
	}
	for _, file := range files {
		if strings.HasPrefix(file.MediaType, "image/") || !strings.HasPrefix(file.MediaType, "text/") {
			return errors.New("automatic context recovery cannot safely estimate media input")
		}
		encoded, marshalErr := json.Marshal(file)
		if marshalErr != nil || !add(len(encoded)) {
			return errors.New("automatic context recovery cannot safely estimate attachment input")
		}
	}
	var recoveryToolBytes int64
	for _, tool := range tools {
		if tool == nil {
			return errors.New("automatic context recovery encountered an unknown tool schema")
		}
		info := tool.Info()
		if info.Name == "" {
			return errors.New("automatic context recovery encountered an unknown tool schema")
		}
		if info.Parameters == nil {
			info.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		encoded, marshalErr := json.Marshal(info)
		if marshalErr != nil || int64(len(encoded)) > math.MaxInt64-recoveryToolBytes {
			return errors.New("automatic context recovery cannot safely estimate tool schemas")
		}
		recoveryToolBytes += int64(len(encoded))
	}
	if !add(int(recoveryToolBytes)) {
		return errors.New("automatic context recovery cannot safely estimate tool schemas")
	}
	if used >= inputBudget {
		return errors.New("automatic context recovery input exceeds the known safe capacity")
	}
	return nil
}

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
	prepared, files := a.preparePrompt(candidate, todos, state.targetModel.Model.Provider(), state.targetAttachments...)
	prepared = a.workaroundProviderMediaLimitations(prepared, state.targetModel)
	prepared = append(prepared, fantasy.NewUserMessage(codexRecoveryNudge))
	prepared = append([]fantasy.Message{fantasy.NewSystemMessage(state.targetSystem)}, prepared...)
	return recoveryInputCapacity(state.targetModel, state.targetMaxOutput, state.targetPrefix, prepared, files, state.targetTools)
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
	addHeartbeatUsage(ctx, model.Model.Provider(), model.Model.Model(), usage, cost)
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

func recoverySummarySystemMessages(aiMsgs []fantasy.Message, prompt string) []fantasy.Message {
	messages := make([]fantasy.Message, 0, len(aiMsgs)+2)
	messages = append(messages, fantasy.NewSystemMessage(string(summaryPrompt)))
	messages = append(messages, aiMsgs...)
	messages = append(messages, fantasy.NewUserMessage(prompt))
	return messages
}
