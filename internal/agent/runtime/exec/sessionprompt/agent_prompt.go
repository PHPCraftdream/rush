// Message shaping around the provider stream: cache-control/context flags
// carried on the request context, history filters for orphaned tool calls
// and superseded supervision notices, context-window trimming, provider
// retry logging fields, and tool-call input sanitisation.
package sessionprompt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/store/message"
)

// autoResumedCtxKey tags a completion turn so its call retains the flag
// across mailbox and durable-queue handoffs.
type autoResumedCtxKey struct{}

// backgroundJobNoticeCtxKey tags a completion turn so its call retains
// the notice flag across mailbox and durable-queue handoffs.
type backgroundJobNoticeCtxKey struct{}

// WithBackgroundJobNotice marks a completion turn as an agent-visible notice.
func WithBackgroundJobNotice(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, autoResumedCtxKey{}, true)
	return context.WithValue(ctx, backgroundJobNoticeCtxKey{}, true)
}

// NoticeFlagsFrom reads the auto-resume and background-job-notice flags
// tagged by WithBackgroundJobNotice.
func NoticeFlagsFrom(ctx context.Context) (autoResumed, backgroundJobNotice bool) {
	autoResumed, _ = ctx.Value(autoResumedCtxKey{}).(bool)
	backgroundJobNotice, _ = ctx.Value(backgroundJobNoticeCtxKey{}).(bool)
	return
}

// drainCallCtxKey tags a turn built through the prompt-string entry points
// (coordinator.Run/buildCall) as a Drain call (phase-4 step 3, doc
// sec.3.4), for callers that cannot build a SessionAgentCall{IsDrain: true}
// literal directly -- today, only the CLI root loop's wake iteration
// (internal/app/app_run_async.go), which drives AgentCoordinator.Run with a
// prompt string through ExecuteRun rather than calling a SessionAgent
// directly the way wakeSession/drainCallFor do. Mirrors WithBackgroundJobNotice's
// pattern exactly: set here, read by buildCall/runInternal, carried on the
// resulting SessionAgentCall from then on like any other field.
type drainCallCtxKey struct{}

// WithDrainCall marks the next prompt-string-driven turn as a Drain call: an
// empty prompt (the caller MUST pass "" -- this alone does not blank an
// existing prompt), no user message persisted, reacting only to whatever
// the turn-start pull moves into history.
func WithDrainCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, drainCallCtxKey{}, true)
}

// IsDrainCallFrom reads the Drain-call flag tagged by WithDrainCall.
func IsDrainCallFrom(ctx context.Context) bool {
	v, _ := ctx.Value(drainCallCtxKey{}).(bool)
	return v
}

// callOriginCtxKey tags a context with the entry-channel origin
// (message.OriginCLI/Web/SDK) of the request that started the turn.
// Set by ExecuteRun (internal/app) and the web server's turn handlers;
// buildCall stamps it onto the SessionAgentCall so the origin is
// persisted on the user message createUserMessage creates.
type callOriginCtxKey struct{}

// WithCallOrigin returns ctx tagged with the entry-channel origin of
// the request. Zero/unspecified origin preserves the historical
// behaviour (no origin recorded).
func WithCallOrigin(ctx context.Context, origin message.Origin) context.Context {
	return context.WithValue(ctx, callOriginCtxKey{}, origin)
}

// CallOriginFrom reads the entry-channel origin tagged by WithCallOrigin
// (message.OriginUnspecified when absent).
func CallOriginFrom(ctx context.Context) message.Origin {
	if ctx == nil {
		return message.OriginUnspecified
	}
	origin, _ := ctx.Value(callOriginCtxKey{}).(message.Origin)
	return origin
}

// filterFileParts removes fantasy.FilePart entries from a slice of message
// parts. Used to strip image attachments from historical user messages when
// the current model does not support them.
func filterFileParts(parts []fantasy.MessagePart) []fantasy.MessagePart {
	filtered := make([]fantasy.MessagePart, 0, len(parts))
	for _, part := range parts {
		if _, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
			continue
		}
		filtered = append(filtered, part)
	}
	return filtered
}

// FilterOrphanedToolResults converts a tool message to a fantasy.Message,
// dropping any tool result parts whose tool_call_id has no matching tool call
// in the known set. An orphaned result causes API validation to fail on every
// subsequent turn, permanently locking the session. Returns the filtered
// message and true if at least one valid part remains.
func FilterOrphanedToolResults(m message.Message, knownToolCallIDs map[string]struct{}) (fantasy.Message, bool) {
	aiMsgs := m.ToAIMessage()
	if len(aiMsgs) == 0 {
		return fantasy.Message{}, false
	}
	var validParts []fantasy.MessagePart
	for _, part := range aiMsgs[0].Content {
		tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
		if !ok {
			validParts = append(validParts, part)
			continue
		}
		if _, known := knownToolCallIDs[tr.ToolCallID]; known {
			validParts = append(validParts, part)
		} else {
			slog.Warn(
				"Dropping orphaned tool result with no matching tool call",
				"tool_call_id", tr.ToolCallID,
			)
		}
	}
	if len(validParts) == 0 {
		return fantasy.Message{}, false
	}
	msg := aiMsgs[0]
	msg.Content = validParts
	return msg, true
}

// SyntheticToolResultsForOrphanedCalls returns a tool message containing
// synthetic tool results for any tool calls in the assistant message that
// have no matching result in knownToolResultIDs. LLM APIs require every
// tool_use to be immediately followed by a tool_result; an interrupted
// session can leave orphaned tool_use blocks that permanently lock the
// conversation. Returns the message and true if any synthetic results were
// produced.
func SyntheticToolResultsForOrphanedCalls(m message.Message, knownToolResultIDs map[string]struct{}) (fantasy.Message, bool) {
	var syntheticParts []fantasy.MessagePart
	for _, tc := range m.ToolCalls() {
		if _, hasResult := knownToolResultIDs[tc.ID]; hasResult {
			continue
		}
		slog.Warn(
			"Injecting synthetic tool result for orphaned tool call",
			"tool_call_id", tc.ID,
			"tool_name", tc.Name,
		)
		syntheticParts = append(syntheticParts, fantasy.ToolResultPart{
			ToolCallID: tc.ID,
			Output: fantasy.ToolResultOutputContentError{
				Error: errors.New("tool call was interrupted and did not produce a result, you may retry this call if the result is still needed"),
			},
		})
	}
	if len(syntheticParts) == 0 {
		return fantasy.Message{}, false
	}
	return fantasy.Message{
		Role:    fantasy.MessageRoleTool,
		Content: syntheticParts,
	}, true
}

// DropSupersededSupervisionNotices keeps only the newest
// NoticeKind==NoticeKindSupervision message in full (design doc §7: only the
// latest summary matters). Older ones stay in place with their text replaced
// by a short marker instead of being removed: removing a user-role message
// can leave two assistant messages adjacent, which some providers reject.
func DropSupersededSupervisionNotices(msgs []message.Message) []message.Message {
	lastIdx := -1
	for i, m := range msgs {
		if m.NoticeKind == NoticeKindSupervision {
			lastIdx = i
		}
	}
	if lastIdx == -1 {
		return msgs
	}
	out := make([]message.Message, len(msgs))
	copy(out, msgs)
	for i, m := range out {
		if m.NoticeKind == NoticeKindSupervision && i != lastIdx {
			m.Parts = []message.ContentPart{message.TextContent{Text: supersededSupervisionText}}
			out[i] = m
		}
	}
	return out
}

// supersededSupervisionText replaces an older supervision summary in the
// model's context.
const supersededSupervisionText = "[Earlier supervision check-in; superseded by a newer one.]"

// TrimMessagesToWindow returns a suffix of msgs whose estimated token count
// fits within targetTokens (1 token ≈ 4 characters).  It always starts on a
// user-role message so the conversation stays well-formed.
func TrimMessagesToWindow(msgs []fantasy.Message, targetTokens int64) []fantasy.Message {
	if len(msgs) == 0 || targetTokens <= 0 {
		return msgs
	}
	const charsPerToken = 4
	budget := int(targetTokens) * charsPerToken

	var accumulated int
	cutIdx := 0 // by default keep everything
	for i := len(msgs) - 1; i >= 0; i-- {
		accumulated += EstimateMsgChars(msgs[i])
		if accumulated >= budget {
			cutIdx = i + 1
			break
		}
	}
	if cutIdx == 0 {
		return msgs // all messages fit
	}
	// Advance to the next user-role message to keep the history well-formed.
	for cutIdx < len(msgs) && msgs[cutIdx].Role != fantasy.MessageRoleUser {
		cutIdx++
	}
	if cutIdx >= len(msgs) {
		return msgs // can't trim without losing all context
	}
	return msgs[cutIdx:]
}

// EstimateMsgChars returns a rough character count for a fantasy.Message,
// used to estimate its token footprint for window trimming.
func EstimateMsgChars(msg fantasy.Message) int {
	total := 0
	for _, part := range msg.Content {
		switch p := part.(type) {
		case fantasy.TextPart:
			total += len(p.Text)
		case fantasy.ToolCallPart:
			total += len(p.ToolName) + len(p.Input)
		case fantasy.ToolResultPart:
			switch o := p.Output.(type) {
			case fantasy.ToolResultOutputContentText:
				total += len(o.Text)
			case fantasy.ToolResultOutputContentError:
				total += len(fmt.Sprintf("%v", o.Error))
			}
		}
	}
	if total == 0 {
		total = 64 // minimum for empty / binary messages
	}
	return total
}

// ProviderRetryLogFields builds the slog fields logged alongside a provider
// retry warning.
func ProviderRetryLogFields(err *fantasy.ProviderError, delay time.Duration) []any {
	fields := []any{
		"retry_delay", delay.String(),
	}
	if err == nil {
		return fields
	}
	fields = append(fields, "status_code", err.StatusCode)
	if err.Title != "" {
		fields = append(fields, "title", err.Title)
	}
	if err.Message != "" {
		fields = append(fields, "message", err.Message)
	}
	return fields
}

// SanitizeToolInput validates tool call JSON from the provider.
// Malformed input — and a bare null payload, which is valid JSON but
// carries no arguments — is replaced with an empty object to prevent
// stuck conversations from truncated or malformed model output.
// The second return value indicates whether sanitization occurred.
func SanitizeToolInput(toolName, toolCallID, input string) (string, bool) {
	if !json.Valid([]byte(input)) {
		slog.Warn("Malformed tool call JSON from provider, replacing with empty object",
			"tool", toolName,
			"id", toolCallID,
			"input_len", len(input),
		)
		return "{}", true
	}
	// A bare null payload is valid JSON but carries no arguments; every
	// consumer treats it the same as an empty object, so normalize it at
	// the source instead of letting a null literal reach the transcript,
	// where non-defensive readers (the web formatter) dereference it.
	if trimmed := strings.TrimSpace(input); trimmed == "null" {
		slog.Warn("Null tool call JSON from provider, replacing with empty object",
			"tool", toolName,
			"id", toolCallID,
		)
		return "{}", true
	}
	return input, false
}
