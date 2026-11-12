package agent

import (
	"encoding/json"
	"errors"
	"fmt"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/discover"
)

func effectiveMaxOutputTokens(model Model, callOverride int64) int64 {
	if callOverride > 0 {
		return callOverride
	}
	if model.ModelCfg.MaxTokens > 0 {
		return model.ModelCfg.MaxTokens
	}
	return model.CatwalkCfg.DefaultMaxTokens
}

// modelInputBudget is the input-token budget the context gate works against:
// window minus output reserve (the whole window when no reserve is known),
// never above the vendor-documented maximum input of the model.
func modelInputBudget(model Model, callOverride int64) int64 {
	cw := int64(model.CatwalkCfg.ContextWindow)
	reserve := effectiveMaxOutputTokens(model, callOverride)
	budget := contextInputBudget(cw, reserve)
	if reserve <= 0 {
		// Legacy gate for models without a known output reserve.
		budget = cw
	}
	id := model.CatwalkCfg.ID
	if id == "" {
		id = model.ModelCfg.Model
	}
	if limit := discover.LookupModelMaxInput(model.ModelCfg.Provider, id); limit > 0 && budget > limit {
		budget = limit
	}
	return budget
}

func contextInputBudget(contextWindow, outputReserve int64) int64 {
	if contextWindow <= 0 || outputReserve <= 0 || outputReserve >= contextWindow {
		return 0
	}
	return contextWindow - outputReserve
}

// contextAutoSummarizeThreshold is the remaining-input headroom at or below
// which the context gate fires. Large windows keep at least
// largeContextWindowBuffer but scale with the budget (1/largeContextHeadroomDivisor),
// so one tool-heavy step cannot jump past a fixed 20k margin on a ~1M window.
func contextAutoSummarizeThreshold(contextWindow, inputBudget int64) int64 {
	if contextWindow <= 0 || inputBudget <= 0 {
		return 0
	}
	threshold := int64(float64(inputBudget) * smallContextWindowRatio)
	if contextWindow > largeContextWindowThreshold {
		threshold = max(largeContextWindowBuffer, inputBudget/largeContextHeadroomDivisor)
	}
	return min(inputBudget, threshold)
}

// unreportedTailTokens conservatively estimates the tokens of messages after
// the last assistant message (tool results, injected or new user input). The
// previous step's usage report cannot include them, so the context gate adds
// them. It uses a stricter chars-per-token ratio than trimMessagesToWindow.
func unreportedTailTokens(msgs []fantasy.Message) int64 {
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == fantasy.MessageRoleAssistant {
			start = i + 1
			break
		}
	}
	var chars int64
	for _, msg := range msgs[start:] {
		chars += int64(estimateMsgChars(msg))
	}
	return (chars + unreportedCharsPerToken - 1) / unreportedCharsPerToken
}

// preparedInputTokenCount retains its token-named signature for runtime callers;
// it returns a UTF-8 JSON-byte upper-bound for message content and prefix, not a
// tokenizer estimate. Callers adding tool schemas to a request must budget them
// separately. Output reserve is accounted for by contextInputBudget.
func preparedInputTokenCount(messages []fantasy.Message, outputReserve int64, systemPrefix string) (int64, error) {
	_ = outputReserve // Output is reserved separately by contextInputBudget.

	serializedMessages := make([]json.RawMessage, 0, len(messages)+1)
	if systemPrefix != "" {
		encodedPart, err := json.Marshal(fantasy.TextPart{Text: systemPrefix})
		if err != nil {
			return 0, fmt.Errorf("cannot serialize system prefix: %w", err)
		}
		encodedMessage, err := json.Marshal(struct {
			Role    fantasy.MessageRole `json:"role"`
			Content []json.RawMessage   `json:"content"`
		}{Role: fantasy.MessageRoleSystem, Content: []json.RawMessage{encodedPart}})
		if err != nil {
			return 0, fmt.Errorf("cannot serialize system prefix message: %w", err)
		}
		serializedMessages = append(serializedMessages, encodedMessage)
	}
	for i, msg := range messages {
		content := make([]json.RawMessage, len(msg.Content))
		for j, part := range msg.Content {
			if part == nil {
				return 0, fmt.Errorf("cannot estimate nil prompt content at message %d part %d", i, j)
			}
			switch value := part.(type) {
			case fantasy.TextPart:
				encoded, err := json.Marshal(value)
				if err != nil {
					return 0, fmt.Errorf("cannot serialize prompt content %T: %w", part, err)
				}
				content[j] = encoded
			case *fantasy.TextPart:
				if value == nil {
					return 0, fmt.Errorf("cannot estimate nil prompt content at message %d part %d", i, j)
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					return 0, fmt.Errorf("cannot serialize prompt content %T: %w", part, err)
				}
				content[j] = encoded
			case fantasy.ReasoningPart:
				encoded, err := json.Marshal(value)
				if err != nil {
					return 0, fmt.Errorf("cannot serialize prompt content %T: %w", part, err)
				}
				content[j] = encoded
			case *fantasy.ReasoningPart:
				if value == nil {
					return 0, fmt.Errorf("cannot estimate nil prompt content at message %d part %d", i, j)
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					return 0, fmt.Errorf("cannot serialize prompt content %T: %w", part, err)
				}
				content[j] = encoded
			case fantasy.ToolCallPart:
				encoded, err := json.Marshal(value)
				if err != nil {
					return 0, fmt.Errorf("cannot serialize prompt content %T: %w", part, err)
				}
				content[j] = encoded
			case *fantasy.ToolCallPart:
				if value == nil {
					return 0, fmt.Errorf("cannot estimate nil prompt content at message %d part %d", i, j)
				}
				encoded, err := json.Marshal(value)
				if err != nil {
					return 0, fmt.Errorf("cannot serialize prompt content %T: %w", part, err)
				}
				content[j] = encoded
			case fantasy.ToolResultPart:
				encoded, err := serializedToolResult(value)
				if err != nil {
					return 0, fmt.Errorf("cannot estimate prompt content at message %d part %d: %w", i, j, err)
				}
				content[j] = encoded
			case *fantasy.ToolResultPart:
				if value == nil {
					return 0, fmt.Errorf("cannot estimate nil prompt content at message %d part %d", i, j)
				}
				encoded, err := serializedToolResult(*value)
				if err != nil {
					return 0, fmt.Errorf("cannot estimate prompt content at message %d part %d: %w", i, j, err)
				}
				content[j] = encoded
			default:
				return 0, fmt.Errorf("cannot estimate opaque prompt content %T", part)
			}
			if content[j] == nil {
				return 0, fmt.Errorf("cannot serialize prompt content %T", part)
			}
		}
		encoded, err := json.Marshal(struct {
			Role            fantasy.MessageRole     `json:"role"`
			Content         []json.RawMessage       `json:"content"`
			ProviderOptions fantasy.ProviderOptions `json:"providerOptions,omitempty"`
		}{Role: msg.Role, Content: content, ProviderOptions: msg.ProviderOptions})
		if err != nil {
			return 0, fmt.Errorf("cannot serialize prompt message %d: %w", i, err)
		}
		serializedMessages = append(serializedMessages, encoded)
	}

	encoded, err := json.Marshal(serializedMessages)
	if err != nil {
		return 0, fmt.Errorf("cannot serialize prompt messages: %w", err)
	}
	return int64(len(encoded)), nil
}

func serializedToolResult(result fantasy.ToolResultPart) (json.RawMessage, error) {
	switch output := result.Output.(type) {
	case fantasy.ToolResultOutputContentText, *fantasy.ToolResultOutputContentText:
		return json.Marshal(result)
	case fantasy.ToolResultOutputContentError:
		if output.Error == nil {
			return nil, errors.New("tool result has nil error")
		}
		return json.Marshal(struct {
			ToolCallID string `json:"toolCallId"`
			Output     string `json:"output"`
		}{ToolCallID: result.ToolCallID, Output: output.Error.Error()})
	case *fantasy.ToolResultOutputContentError:
		if output == nil || output.Error == nil {
			return nil, errors.New("tool result has nil error")
		}
		return json.Marshal(struct {
			ToolCallID string `json:"toolCallId"`
			Output     string `json:"output"`
		}{ToolCallID: result.ToolCallID, Output: output.Error.Error()})
	default:
		return nil, fmt.Errorf("unsupported tool result output %T", result.Output)
	}
}
