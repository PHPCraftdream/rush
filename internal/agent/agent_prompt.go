// Message shaping around the provider stream: history/prompt assembly
// (preparePrompt and its orphaned-tool-call filters), user-message creation,
// cache-control options and session headers, tool-result conversion and
// provider media workarounds, context-window trimming, and input sanitisation.
package agent

import (
	"context"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/sessionprompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	core "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

func (a *sessionAgent) getCacheControlOptions() fantasy.ProviderOptions {
	return (sessionprompt.PromptComponent{}).GetCacheControlOptions()
}

func (a *sessionAgent) createUserMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	return (sessionprompt.PromptComponent{Messages: a.messages}).CreateUserMessage(ctx, call)
}

func (a *sessionAgent) preparePrompt(msgs []message.Message, todos []core.Todo, targetFamily string, attachments ...message.Attachment) ([]fantasy.Message, []fantasy.FilePart) {
	return (sessionprompt.PromptComponent{IsSubAgent: a.isSubAgent}).PreparePrompt(msgs, todos, targetFamily, attachments...)
}

func (a *sessionAgent) getSessionMessages(ctx context.Context, session core.Session) ([]message.Message, error) {
	return (sessionprompt.PromptComponent{Messages: a.messages}).GetSessionMessages(ctx, session)
}

func (a *sessionAgent) convertToToolResult(result fantasy.ToolResultContent) message.ToolResult {
	return (sessionprompt.PromptComponent{}).ConvertToToolResult(result)
}

func (a *sessionAgent) workaroundProviderMediaLimitations(messages []fantasy.Message, smartModel Model) []fantasy.Message {
	return (sessionprompt.PromptComponent{}).WorkaroundProviderMediaLimitations(messages, smartModel)
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
