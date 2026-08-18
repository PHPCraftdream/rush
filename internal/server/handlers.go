package server

import (
	"context"
	"encoding/json"
	"log/slog"

	appPkg "github.com/charmbracelet/crush/internal/app"
)

// handleIncoming dispatches an incoming WS message from a client.
// Most operations are launched in goroutines via c.dispatch (hub.go), which
// recovers panics inside the handler and bounds how many handlers may run
// concurrently for this connection — see maxConcurrentHandlersPerConn.
// handleIncoming itself never blocks except for the brief semaphore acquire
// inside dispatch once that cap is hit.
//
// Control-plane commands (CmdCancelAgent, CmdInterruptAndSend) are the
// exception: they go through c.dispatchControl instead, which uses a
// separate, much larger semaphore. Both handlers are fast — they signal
// cancellation via an in-memory map lookup and/or a bounded DB read, never
// AgentCoordinator.Run — and must not be able to queue behind
// maxConcurrentHandlersPerConn long-running turns (e.g. handleSendMessage),
// since that's exactly when a user most needs cancel/interrupt to go
// through promptly. See dispatchControl's doc comment for the full story.
func handleIncoming(ctx context.Context, a *appPkg.App, c *Client, raw []byte) {
	var msg WSMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Debug("ws: malformed message", "err", err)
		c.reply("", EventError, nil, "malformed message")
		return
	}

	switch msg.Type {
	case CmdSendMessage:
		c.dispatch("handleSendMessage", func() { handleSendMessage(ctx, a, c, msg) })
	case CmdInterruptAndSend:
		// Control-plane: must cancel an in-flight (possibly stuck/long-running)
		// turn promptly, so it cannot share sem with handleSendMessage — see
		// dispatchControl's doc comment in hub.go for the full rationale.
		c.dispatchControl("handleInterruptAndSend", func() { handleInterruptAndSend(ctx, a, c, msg) })
	case CmdInjectMessage:
		c.dispatch("handleInjectMessage", func() { handleInjectMessage(ctx, a, c, msg) })
	case CmdCancelAgent:
		// Control-plane: same rationale as CmdInterruptAndSend above.
		c.dispatchControl("handleCancelAgent", func() { handleCancelAgent(ctx, a, c, msg) })
	case CmdCreateSession:
		c.dispatch("handleCreateSession", func() { handleCreateSession(ctx, a, c, msg) })
	case CmdForkSession:
		c.dispatch("handleForkSession", func() { handleForkSession(ctx, a, c, msg) })
	case CmdDeleteSession:
		c.dispatch("handleDeleteSession", func() { handleDeleteSession(ctx, a, c, msg) })
	case CmdDeleteOtherSessions:
		c.dispatch("handleDeleteOtherSessions", func() { handleDeleteOtherSessions(ctx, a, c, msg) })
	case CmdListSessions:
		c.dispatch("handleListSessions", func() { handleListSessions(ctx, a, c, msg) })
	case CmdLoadMessages:
		c.dispatch("handleLoadMessages", func() { handleLoadMessages(ctx, a, c, msg) })
	case CmdGetConfig:
		c.dispatch("handleGetConfig", func() { handleGetConfig(a, c, msg) })
	case CmdGetLogs:
		c.dispatch("handleGetLogs", func() { handleGetLogs(a, c, msg) })
	case CmdSetTheme:
		c.dispatch("handleSetTheme", func() { handleSetTheme(a, c, msg) })
	case CmdSetKeepAlive:
		c.dispatch("handleSetKeepAlive", func() { handleSetKeepAlive(a, c, msg) })
	case CmdRenameSession:
		c.dispatch("handleRenameSession", func() { handleRenameSession(ctx, a, c, msg) })
	case CmdSetSessionModels:
		c.dispatch("handleSetSessionModels", func() { handleSetSessionModels(ctx, a, c, msg) })
	case CmdRemoveRecentModel:
		c.dispatch("handleRemoveRecentModel", func() { handleRemoveRecentModel(a, c, msg) })
	case CmdTrackModelUsage:
		c.dispatch("handleTrackModelUsage", func() { handleTrackModelUsage(a, c, msg) })
	case CmdGetScopedModels:
		c.dispatch("handleGetScopedModels", func() { handleGetScopedModels(a, c, msg) })
	case CmdSetScopedModel:
		c.dispatch("handleSetScopedModel", func() { handleSetScopedModel(a, c, msg) })
	case CmdClearScopedModel:
		c.dispatch("handleClearScopedModel", func() { handleClearScopedModel(a, c, msg) })
	case CmdSetProviderKey:
		c.dispatch("handleSetProviderKey", func() { handleSetProviderKey(a, c, msg) })
	case CmdRemoveProviderKey:
		c.dispatch("handleRemoveProviderKey", func() { handleRemoveProviderKey(a, c, msg) })
	case CmdDeleteMessage:
		c.dispatch("handleDeleteMessage", func() { handleDeleteMessage(ctx, a, c, msg) })
	case CmdDeleteMessages:
		c.dispatch("handleDeleteMessages", func() { handleDeleteMessages(ctx, a, c, msg) })
	case CmdUpdateMessageContent:
		c.dispatch("handleUpdateMessageContent", func() { handleUpdateMessageContent(ctx, a, c, msg) })
	case CmdUpdateMessageThinking:
		c.dispatch("handleUpdateMessageThinking", func() { handleUpdateMessageThinking(ctx, a, c, msg) })
	case CmdGetSystemPrompt:
		c.dispatch("handleGetSystemPrompt", func() { handleGetSystemPrompt(ctx, a, c, msg) })
	case CmdSetSystemPrompt:
		c.dispatch("handleSetSystemPrompt", func() { handleSetSystemPrompt(ctx, a, c, msg) })
	case CmdSummarizeSession:
		c.dispatch("handleSummarizeSession", func() { handleSummarizeSession(ctx, a, c, msg) })
	case CmdCancelQueuedSummarize:
		c.dispatch("handleCancelQueuedSummarize", func() { handleCancelQueuedSummarize(a, c, msg) })
	case CmdDeleteMessagePart:
		c.dispatch("handleDeleteMessagePart", func() { handleDeleteMessagePart(ctx, a, c, msg) })
	case CmdUpdateMessagePart:
		c.dispatch("handleUpdateMessagePart", func() { handleUpdateMessagePart(ctx, a, c, msg) })
	case CmdTogglePinMessage:
		c.dispatch("handleTogglePinMessage", func() { handleTogglePinMessage(ctx, a, c, msg) })
	case CmdRerunMessage:
		c.dispatch("handleRerunMessage", func() { handleRerunMessage(ctx, a, c, msg) })
	case CmdLogClientEvent:
		c.dispatch("handleLogClientEvent", func() { handleLogClientEvent(a, c, msg) })
	case CmdLogClientError:
		c.dispatch("handleLogClientError", func() { handleLogClientError(c, msg) })
	case CmdSetMCPDisabled:
		c.dispatch("handleSetMCPDisabled", func() { handleSetMCPDisabled(ctx, a, c, msg) })
	case CmdAddMCPServer:
		c.dispatch("handleAddMCPServer", func() { handleAddMCPServer(ctx, a, c, msg) })
	case CmdRemoveMCPServer:
		c.dispatch("handleRemoveMCPServer", func() { handleRemoveMCPServer(a, c, msg) })
	case CmdUpdateMCPServer:
		c.dispatch("handleUpdateMCPServer", func() { handleUpdateMCPServer(ctx, a, c, msg) })
	case CmdSetDebug:
		c.dispatch("handleSetDebug", func() { handleSetDebug(a, c, msg) })
	case CmdAddContextPath:
		c.dispatch("handleAddContextPath", func() { handleAddContextPath(a, c, msg) })
	case CmdRemoveContextPath:
		c.dispatch("handleRemoveContextPath", func() { handleRemoveContextPath(a, c, msg) })
	case CmdGetSkills:
		c.dispatch("handleGetSkills", func() { handleGetSkills(a, c, msg) })
	case CmdAddSkillsPath:
		c.dispatch("handleAddSkillsPath", func() { handleAddSkillsPath(a, c, msg) })
	case CmdRemoveSkillsPath:
		c.dispatch("handleRemoveSkillsPath", func() { handleRemoveSkillsPath(a, c, msg) })
	case CmdInitializeProject:
		c.dispatch("handleInitializeProject", func() { handleInitializeProject(ctx, a, c, msg) })
	case CmdAddCustomProvider:
		c.dispatch("handleAddCustomProvider", func() { handleAddCustomProvider(a, c, msg) })
	case CmdRemoveCustomProvider:
		c.dispatch("handleRemoveCustomProvider", func() { handleRemoveCustomProvider(a, c, msg) })
	case CmdUpdateCustomProvider:
		c.dispatch("handleUpdateCustomProvider", func() { handleUpdateCustomProvider(a, c, msg) })
	case CmdSetProviderPeakHours:
		c.dispatch("handleSetProviderPeakHours", func() { handleSetProviderPeakHours(a, c, msg) })
	case CmdUpdateTodos:
		c.dispatch("handleUpdateTodos", func() { handleUpdateTodos(ctx, a, c, msg) })
	default:
		slog.Debug("ws: unknown command", "type", msg.Type)
		c.reply(msg.ID, EventError, nil, "unknown command: "+msg.Type)
	}
}

// autoApproveWebSession marks a session for blanket permission auto-approval.
// In the web UI there is no permission dialog — the agent must never block
// waiting for a user to grant/deny a tool call. This mirrors the
// non-interactive `crush run` path (app.RunNonInteractive →
// Permissions.AutoApproveSession). It is idempotent: re-arming an
// already-approved session is a no-op. The restricted-run allowlist is NOT
// armed here (unlike `crush run`), so approval is unconditional.
func autoApproveWebSession(a *appPkg.App, sessionID string) {
	if a == nil || a.Permissions == nil || sessionID == "" {
		return
	}
	a.Permissions.AutoApproveSession(sessionID)
}

func handleLogClientEvent(a *appPkg.App, c *Client, msg WSMessage) {
	cfg := a.Config()
	if cfg == nil || !cfg.Options.Debug {
		return
	}
	var p LogClientEventPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return
	}
	slog.Debug("client event", "event", p.Event, "details", p.Details)
}

func handleLogClientError(c *Client, msg WSMessage) {
	var p LogClientErrorPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		return
	}
	slog.Error("client error", "message", p.Message, "source", p.Source, "stack", p.Stack)
}
