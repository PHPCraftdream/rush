// Message shaping around the provider stream: history/prompt assembly
// (preparePrompt and its orphaned-tool-call filters), user-message creation,
// cache-control options and session headers, tool-result conversion and
// provider media workarounds, context-window trimming, and input sanitisation.
package agent

import (
	"context"

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
