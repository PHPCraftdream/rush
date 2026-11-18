// Message shaping around the provider stream: history/prompt assembly
// (preparePrompt and its orphaned-tool-call filters), user-message creation,
// cache-control options and session headers, tool-result conversion and
// provider media workarounds, context-window trimming, and input sanitisation.
package agent

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/sessionprompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/kit/text/stringext"
	core "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

func (a *sessionAgent) getCacheControlOptions() fantasy.ProviderOptions {
	return (sessionPromptComponent{}).getCacheControlOptions()
}

func (a *sessionAgent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	return (sessionPromptComponent{messages: a.messages}).createUserMessage(ctx, call)
}

func (a *sessionAgent) preparePrompt(msgs []message.Message, todos []core.Todo, targetFamily string, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart) {
	return (sessionPromptComponent{isSubAgent: a.isSubAgent}).preparePrompt(msgs, todos, targetFamily, attachments...)
}

func (a *sessionAgent) getSessionMessages(ctx context.Context, session core.Session) ([]message.Message, error) {
	return (sessionPromptComponent{messages: a.messages}).getSessionMessages(ctx, session)
}

func (a *sessionAgent) convertToToolResult(result fantasy.ToolResultContent) message.ToolResult {
	return (sessionPromptComponent{}).convertToToolResult(result)
}

func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, smartModel Model) []fantasy.Message {
	return (sessionPromptComponent{}).workaroundProviderMediaLimitations(messages, smartModel)
}

// sessionHeaders returns the HTTP headers we use for cache affinity on
// every LLM request for a given session.
//
// Thin delegate: implementation lives in
// internal/agent/runtime/policy/provauth.SessionHeaders.
func sessionHeaders(sessionID string) map[string]string {
	return provauth.SessionHeaders(sessionID)
}

// WithBackgroundJobNotice marks a completion turn as an agent-visible notice.
func WithBackgroundJobNotice(ctx context.Context) context.Context {
	return sessionprompt.WithBackgroundJobNotice(ctx)
}

func noticeFlagsFrom(ctx context.Context) (autoResumed, backgroundJobNotice bool) {
	return sessionprompt.NoticeFlagsFrom(ctx)
}

// WithDrainCall marks the next prompt-string-driven turn as a Drain call: an
// empty prompt (the caller MUST pass "" -- this alone does not blank an
// existing prompt), no user message persisted, reacting only to whatever
// the turn-start pull moves into history.
func WithDrainCall(ctx context.Context) context.Context {
	return sessionprompt.WithDrainCall(ctx)
}

func isDrainCallFrom(ctx context.Context) bool {
	return sessionprompt.IsDrainCallFrom(ctx)
}

// WithCallOrigin returns ctx tagged with the entry-channel origin of
// the request. Zero/unspecified origin preserves the historical
// behaviour (no origin recorded).
func WithCallOrigin(ctx context.Context, origin message.Origin) context.Context {
	return sessionprompt.WithCallOrigin(ctx, origin)
}

// CallOriginFrom reads the entry-channel origin tagged by WithCallOrigin
// (message.OriginUnspecified when absent).
func CallOriginFrom(ctx context.Context) message.Origin {
	return sessionprompt.CallOriginFrom(ctx)
}

func filterOrphanedToolResults(m message.Message, knownToolCallIDs map[string]struct{}) (fantasy.Message, bool) {
	return sessionprompt.FilterOrphanedToolResults(m, knownToolCallIDs)
}

func syntheticToolResultsForOrphanedCalls(m message.Message, knownToolResultIDs map[string]struct{}) (fantasy.Message, bool) {
	return sessionprompt.SyntheticToolResultsForOrphanedCalls(m, knownToolResultIDs)
}

func dropSupersededSupervisionNotices(msgs []message.Message) []message.Message {
	return sessionprompt.DropSupersededSupervisionNotices(msgs)
}

func trimMessagesToWindow(msgs []fantasy.Message, targetTokens int64) []fantasy.Message {
	return sessionprompt.TrimMessagesToWindow(msgs, targetTokens)
}

func estimateMsgChars(msg fantasy.Message) int {
	return sessionprompt.EstimateMsgChars(msg)
}

func providerRetryLogFields(err *fantasy.ProviderError, delay time.Duration) []any {
	return sessionprompt.ProviderRetryLogFields(err, delay)
}

func sanitizeToolInput(toolName, toolCallID, input string) (string, bool) {
	return sessionprompt.SanitizeToolInput(toolName, toolCallID, input)
}

type sessionPromptComponent struct {
	messages   message.Service
	isSubAgent bool
}

func (sessionPromptComponent) getCacheControlOptions() fantasy.ProviderOptions {
	if t, _ := strconv.ParseBool(os.Getenv("RUSH_DISABLE_ANTHROPIC_CACHE")); t {
		return fantasy.ProviderOptions{}
	}
	opts := fantasy.ProviderOptions{}
	for _, provider := range provauth.ExplicitCacheControlProviders() {
		opts[provider] = &anthropic.ProviderCacheControlOptions{
			CacheControl: anthropic.CacheControl{Type: "ephemeral"},
		}
	}
	return opts
}

func (p sessionPromptComponent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	parts := []message.ContentPart{message.TextContent{Text: call.Prompt}}
	var attachmentParts []message.ContentPart
	for _, attachment := range call.Attachments {
		attachmentParts = append(attachmentParts, message.BinaryContent{Path: attachment.FilePath, MIMEType: attachment.MimeType, Data: attachment.Content})
	}
	parts = append(parts, attachmentParts...)
	msg, err := p.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
		Role:                message.User,
		Parts:               parts,
		AutoResumed:         call.AutoResumed,
		BackgroundJobNotice: call.BackgroundJobNotice,
		NoticeKind:          call.NoticeKind,
		Origin:              call.Origin,
	})
	if err != nil {
		return message.Message{}, fmt.Errorf("failed to create user message: %w", err)
	}
	return msg, nil
}

func (p sessionPromptComponent) preparePrompt(msgs []message.Message, todos []core.Todo, targetFamily string, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart) {
	var history []fantasy.Message
	var reminderMsg fantasy.Message
	hasReminder := false
	if !p.isSubAgent {
		// Fork merge note: we already extended this block to also surface the
		// CURRENT todo list when non-empty (originally only handled empty).
		// Upstream's small reword to the empty-case text is not worth the churn.
		var reminderText string
		if len(todos) == 0 {
			reminderText = `This is a reminder that your todo list is currently empty — all previous tasks have been completed or deleted. DO NOT recreate any old tasks from memory. DO NOT mention this to the user explicitly because they are already aware.
If you are working on tasks that would benefit from a todo list please use the "todos" tool to create one.
If not, please feel free to ignore. Again do not mention this message to the user.`
		} else {
			var sb strings.Builder
			sb.WriteString("This is a reminder of your CURRENT todo list. This is the authoritative ground truth — it overrides anything in your conversation history:\n\n")
			for _, t := range todos {
				fmt.Fprintf(&sb, "- [%s] %s\n", t.Status, t.Content)
			}
			sb.WriteString("\nIMPORTANT: Tasks NOT in this list have been DELETED (by the user or by you). Do NOT add them back. Only manage the tasks listed above, plus any new ones the user explicitly requests. DO NOT mention this reminder to the user.")
			reminderText = sb.String()
		}
		reminderMsg = fantasy.NewUserMessage(
			fmt.Sprintf("<system_reminder>%s</system_reminder>", reminderText),
		)
		hasReminder = true
	}
	// A user message created while a tool ran must not split tool_use from
	// its tool_result (#1066).
	msgs = sessionprompt.DeferUserMessagesPastToolRound(msgs)
	// Collect all tool call IDs present in assistant messages and all tool
	// result IDs present in tool messages. This lets us detect both orphaned
	// tool results (result without a call) and orphaned tool calls (call
	// without a result).
	knownToolCallIDs := make(map[string]struct{})
	knownToolResultIDs := make(map[string]struct{})
	for _, m := range msgs {
		switch m.Role {
		case message.Assistant:
			for _, tc := range m.ToolCalls() {
				knownToolCallIDs[tc.ID] = struct{}{}
			}
		case message.Tool:
			for _, tr := range m.ToolResults() {
				knownToolResultIDs[tr.ToolCallID] = struct{}{}
			}
		}
	}

	for _, m := range msgs {
		if len(m.Parts) == 0 {
			continue
		}
		// Assistant message without content or tool calls (cancelled before it returned anything).
		if m.Role == message.Assistant && len(m.ToolCalls()) == 0 && m.Content().Text == "" && m.FullReasoningText() == "" {
			continue
		}
		if m.Role == message.Tool {
			if msg, ok := filterOrphanedToolResults(m, knownToolCallIDs); ok {
				history = append(history, msg)
			}
			continue
		}
		aiMsgs := m.ToAIMessageFor(targetFamily)
		// Fork merge note (origin/main 6d95ecc5 "skip image attachments in
		// history when model doesn't support them"): we intentionally skip
		// upstream's per-message filter here — the same scrub happens in
		// workaroundProviderMediaLimitations() which runs once per Stream
		// call inside PrepareStep, so doing it twice would just walk the
		// history twice.
		history = append(history, aiMsgs...)

		if m.Role == message.Assistant {
			if msg, ok := syntheticToolResultsForOrphanedCalls(m, knownToolResultIDs); ok {
				history = append(history, msg)
			}
		}
	}

	// Reminder goes last, not first: keeping it at the tail preserves the
	// longest-common-prefix cache match across turns (PrepareStep caches the
	// last 2 messages; a volatile message near the front busts the whole
	// history's cache on every todo mutation).
	if hasReminder {
		history = append(history, reminderMsg)
	}

	var files []fantasy.FilePart
	for _, attachment := range attachments {
		if attachment.IsText() {
			continue
		}
		files = append(files, fantasy.FilePart{
			Filename:  attachment.FileName,
			Data:      attachment.Content,
			MediaType: attachment.MimeType,
		})
	}

	return history, files
}

func (p sessionPromptComponent) getSessionMessages(ctx context.Context, session core.Session) ([]message.Message, error) {
	msgs, err := p.messages.List(ctx, session.ID)
	if err != nil {
		return nil, fmt.Errorf("failed to list messages: %w", err)
	}

	if session.SummaryMessageID != "" {
		summaryMsgIndex := -1
		for i, msg := range msgs {
			if msg.ID == session.SummaryMessageID {
				summaryMsgIndex = i
				break
			}
		}
		if summaryMsgIndex != -1 {
			// Summary MUST sort before the verbatim tail: the reader returns
			// msgs[summaryIndex:] and treats that summary as the context prefix.
			// Collect pinned messages that appear before the summary
			var pinned []message.Message
			for _, msg := range msgs[:summaryMsgIndex] {
				if msg.Pinned {
					pinned = append(pinned, msg)
				}
			}
			msgs = msgs[summaryMsgIndex:]
			msgs[0].Role = message.User
			if len(pinned) > 0 {
				msgs = append(pinned, msgs...)
			}
		}
	}
	return dropSupersededSupervisionNotices(msgs), nil
}

// convertToToolResult converts a fantasy tool result to a message tool result.
func (sessionPromptComponent) convertToToolResult(result fantasy.ToolResultContent) message.ToolResult {
	baseResult := message.ToolResult{
		ToolCallID: result.ToolCallID,
		Name:       result.ToolName,
		Metadata:   result.ClientMetadata,
	}

	switch result.Result.GetType() {
	case fantasy.ToolResultContentTypeText:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result); ok {
			baseResult.Content = r.Text
		}
	case fantasy.ToolResultContentTypeError:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); ok {
			baseResult.Content = r.Error.Error()
			baseResult.IsError = true
		}
	case fantasy.ToolResultContentTypeMedia:
		if r, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Result); ok {
			if !stringext.IsValidBase64(r.Data) {
				slog.Warn(
					"Tool returned media with invalid base64 data, discarding image",
					"tool", result.ToolName,
					"tool_call_id", result.ToolCallID,
				)
				baseResult.Content = "Tool returned image data with invalid encoding"
				baseResult.IsError = true
			} else {
				content := r.Text
				if content == "" {
					content = fmt.Sprintf("Loaded %s content", r.MediaType)
				}
				baseResult.Content = content
				baseResult.Data = r.Data
				baseResult.MIMEType = r.MediaType
			}
		}
	}

	return baseResult
}

// workaroundProviderMediaLimitations converts media content in tool results to
// user messages for providers that don't natively support images in tool results.
//
// Problem: OpenAI, Google, OpenRouter, and other OpenAI-compatible providers
// don't support sending images/media in tool result messages - they only accept
// text in tool results. However, they DO support images in user messages.
//
// If we send media in tool results to these providers, the API returns an error.
//
// Solution: For these providers, we:
//  1. Replace the media in the tool result with a text placeholder
//  2. Inject a user message immediately after with the image as a file attachment
//  3. This maintains the tool execution flow while working around API limitations
//
// Anthropic and Bedrock support images natively in tool results, so we skip
// this workaround for them.
//
// Example transformation:
//
//	BEFORE: [tool result: image data]
//	AFTER:  [tool result: "Image loaded - see attached"], [user: image attachment]
func (sessionPromptComponent) workaroundProviderMediaLimitations(messages []fantasy.Message, smartModel Model) []fantasy.Message {
	providerSupportsMedia := smartModel.ModelCfg.Provider == string(catwalk.InferenceProviderAnthropic) ||
		smartModel.ModelCfg.Provider == string(catwalk.InferenceProviderBedrock)

	if providerSupportsMedia {
		return messages
	}

	supportsImages := smartModel.CatwalkCfg.SupportsImages

	convertedMessages := make([]fantasy.Message, 0, len(messages))

	for _, msg := range messages {
		if msg.Role != fantasy.MessageRoleTool {
			convertedMessages = append(convertedMessages, msg)
			continue
		}

		textParts := make([]fantasy.MessagePart, 0, len(msg.Content))
		var mediaFiles []fantasy.FilePart

		for _, part := range msg.Content {
			toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				textParts = append(textParts, part)
				continue
			}

			if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok {
				if !supportsImages {
					// Model cannot process images. Replace with a text
					// placeholder and skip creating a synthetic user
					// message with FilePart, which would brick the
					// session on text-only models.
					textParts = append(textParts, fantasy.ToolResultPart{
						ToolCallID: toolResult.ToolCallID,
						Output: fantasy.ToolResultOutputContentText{
							Text: "[Image/media content not supported by this model]",
						},
						ProviderOptions: toolResult.ProviderOptions,
					})
					continue
				}

				decoded, err := base64.StdEncoding.DecodeString(media.Data)
				if err != nil {
					slog.Warn("Failed to decode media data", "error", err)
					textParts = append(textParts, part)
					continue
				}

				mediaFiles = append(mediaFiles, fantasy.FilePart{
					Data:      decoded,
					MediaType: media.MediaType,
					Filename:  fmt.Sprintf("tool-result-%s", toolResult.ToolCallID),
				})

				textParts = append(textParts, fantasy.ToolResultPart{
					ToolCallID: toolResult.ToolCallID,
					Output: fantasy.ToolResultOutputContentText{
						Text: "[Image/media content loaded - see attached file]",
					},
					ProviderOptions: toolResult.ProviderOptions,
				})
			} else {
				textParts = append(textParts, part)
			}
		}

		convertedMessages = append(convertedMessages, fantasy.Message{
			Role:    fantasy.MessageRoleTool,
			Content: textParts,
		})

		if len(mediaFiles) > 0 {
			convertedMessages = append(convertedMessages, fantasy.NewUserMessage(
				"Here is the media content from the tool result:",
				mediaFiles...,
			))
		}
	}

	return convertedMessages
}
