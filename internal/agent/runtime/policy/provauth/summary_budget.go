// Summary-budget estimation: conservative token accounting for unreported
// message tails and serialized prompt sizes used by the summarization gate.
package provauth

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

func AddStepUsageTokens(total int64, usage fantasy.Usage) int64 {
	partsTotal := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens + usage.OutputTokens
	if usage.TotalTokens > partsTotal {
		partsTotal = usage.TotalTokens
	}
	return total + partsTotal
}

// UpdateSessionTokenCounters writes a new usage snapshot into the session
// without overwriting accumulated counters with zero. Fork merge note: from
// origin/main 74e6e378 "fix(agent): harden fallback usage accounting".
//
// PromptTokens must be the FULL prompt: InputTokens, CacheReadTokens and
// CacheCreationTokens are three disjoint classes (see
// internal/agent/cliprovider/usage.go), so all three belong in the sum.
//
// This used to add only InputTokens + CacheReadTokens, silently dropping
// cache-WRITE tokens. That understated the prompt for every provider that
// reports the three separately — the Anthropic HTTP provider always did
// (fantasy anthropic.go maps input_tokens exclusive of both cache counters),
// and claude-cli joined it once its parser stopped folding cache into input.
// A real measured turn had input=5842 / cache_creation=16984 / cache_read=0:
// the prompt is 22826 tokens but was recorded as 5842, a 74% understatement.
//
// PromptTokens drives the auto-summarization trigger (the remaining-context
// checks against CatwalkCfg.ContextWindow), so understating it delays
// compaction and risks running the context window over instead of sliding it.
func UpdateSessionTokenCounters(session *coresession.Session, usage fantasy.Usage) {
	if usage.OutputTokens != 0 {
		session.CompletionTokens = usage.OutputTokens
	}
	promptTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	if promptTokens != 0 {
		session.PromptTokens = promptTokens
	}
}

// ApproxTokenCount is the shared rough token estimator (the usage package
// receives it injected via fallbackStepUsage).
func ApproxTokenCount(s string) int64 {
	if s == "" {
		return 0
	}
	return int64((len(s) + 3) / 4)
}

// SummaryCompletionTokens returns OutputTokens when the provider reported
// them, otherwise falls back to an approximate count from the rendered
// summary message. Fork merge note: from origin/main 6ed8852b
// "fix(agent): estimate missing streamed usage" — used in Summarize when
// the provider omits final usage on the summary stream.
func SummaryCompletionTokens(usage fantasy.Usage, summaryMessage message.Message) int64 {
	if usage.OutputTokens != 0 {
		return usage.OutputTokens
	}
	return ApproxTokenCount(summaryMessage.Content().Text) + ApproxTokenCount(summaryMessage.FullReasoningText())
}

// SummaryCommitMaxDuration bounds the silent compaction's COMMIT phase — the
// SetSummaryAndUsage write plus the deletion of the messages it replaces
// (P0-4 review follow-up).
//
// That phase deliberately runs on context.WithoutCancel: interrupting it
// between "summary pointer written" and "old messages deleted" is how a
// session's history gets unrecoverably holed, so cancellation must not be
// able to land in the middle. Detaching from cancellation without a deadline
// would recreate the unbounded-operation-holds-Run shape P1-B removed, hence
// this bound. The provider stream is already finished by then; what remains
// is local single-writer DB work, so 30s is generous rather than tight.
const SummaryCommitMaxDuration = 30 * time.Second

//go:embed templates/summary.md
var summaryPrompt []byte

// SummaryPromptText returns the embedded summary prompt bytes as a string.
// It is a lazy accessor: callers must evaluate it at the point of use so the
// asset is never materialized eagerly into a global.
func SummaryPromptText() string {
	return string(summaryPrompt)
}

// BuildSummaryPrompt constructs the prompt text for session summarization.
func BuildSummaryPrompt(todos []coresession.Todo) string {
	var sb strings.Builder
	sb.WriteString("Provide a detailed summary of our conversation above.")
	if len(todos) > 0 {
		sb.WriteString("\n\n## Current Todo List\n\n")
		for _, t := range todos {
			fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
		}
		sb.WriteString("\nInclude these tasks and their statuses in your summary. ")
		sb.WriteString("Instruct the resuming assistant to use the `todos` tool to continue tracking progress on these tasks.")
	}
	return sb.String()
}

// unreportedCharsPerToken is the conservative ratio used for content
// appended since the last usage report (trimming assumes 4).
const unreportedCharsPerToken = 3

// UnreportedTailTokens conservatively estimates the tokens of messages after
// the last assistant message (tool results, injected or new user input). The
// previous step's usage report cannot include them, so the context gate adds
// them. It uses a stricter chars-per-token ratio than trimMessagesToWindow.
// estimateChars is the caller's message-size estimator.
func UnreportedTailTokens(msgs []fantasy.Message, estimateChars func(fantasy.Message) int) int64 {
	start := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == fantasy.MessageRoleAssistant {
			start = i + 1
			break
		}
	}
	var chars int64
	for _, msg := range msgs[start:] {
		chars += int64(estimateChars(msg))
	}
	return (chars + unreportedCharsPerToken - 1) / unreportedCharsPerToken
}

// PreparedInputTokenCount retains its token-named signature for runtime callers;
// it returns a UTF-8 JSON-byte upper-bound for message content and prefix, not a
// tokenizer estimate. Callers adding tool schemas to a request must budget them
// separately. Output reserve is accounted for by contextInputBudget.
func PreparedInputTokenCount(messages []fantasy.Message, outputReserve int64, systemPrefix string) (int64, error) {
	_ = outputReserve // Output is reserved separately by ContextInputBudget.

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
