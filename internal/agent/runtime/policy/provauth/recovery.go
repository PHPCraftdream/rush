// Recovery summary capacity and cleanup policy (chunk E): the COMMIT-phase
// cleanup helper, the conservative prepared-input capacity check used by
// automatic context recovery, and the recovery summary message assembly.
package provauth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// CleanupUncommittedSummary deletes a summary message whose COMMIT phase
// failed, joining the cause into the returned error. It runs on the same
// detached, bounded context as the COMMIT phase itself so cancellation
// cannot strand an uncommitted summary.
func CleanupUncommittedSummary(messages message.Service, ctx context.Context, id string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), SummaryCommitMaxDuration)
	defer cancel()
	return errors.Join(cause, messages.Delete(cleanupCtx, id))
}

// RecoveryInputCapacity uses serialized UTF-8 bytes as a conservative upper
// bound, including files and every tool's advertised name/schema/description.
func RecoveryInputCapacity(model agentcall.Model, outputOverride int64, prefix string, messages []fantasy.Message, files []fantasy.FilePart, tools []fantasy.AgentTool) error {
	if model.Model == nil || int64(model.CatwalkCfg.ContextWindow) <= 0 || EffectiveMaxOutputTokens(model, outputOverride) <= 0 {
		return errors.New("automatic context recovery requires known context and output limits")
	}
	inputBudget := ModelInputBudget(model, outputOverride)
	if inputBudget <= 0 {
		return errors.New("automatic context recovery requires known context and output limits")
	}
	used, err := PreparedInputTokenCount(messages, 0, prefix)
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
			return errors.New("automatic context recovery encountered an unknown tool schema")
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

// RecoverySummarySystemMessages assembles the silent recovery summarization
// request: the embedded summary system prompt, the prepared history, then
// the built prompt as the closing user message.
func RecoverySummarySystemMessages(aiMsgs []fantasy.Message, prompt string) []fantasy.Message {
	messages := make([]fantasy.Message, 0, len(aiMsgs)+2)
	messages = append(messages, fantasy.NewSystemMessage(SummaryPromptText()))
	messages = append(messages, aiMsgs...)
	messages = append(messages, fantasy.NewUserMessage(prompt))
	return messages
}
