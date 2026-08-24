package server

import (
	"context"
	"encoding/json"
	"log/slog"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
)

// handleIncoming dispatches an incoming WS message from a client.
// Most operations are launched via c.dispatch (hub.go), which
// recovers panics inside the handler and bounds how many handlers may run
// concurrently for this connection — see maxConcurrentHandlersPerConn.
// handleIncoming itself never blocks: dispatch enqueues into the
// connection's bounded work queue non-blockingly and, when that queue is
// also full, replies with an overload error instead of waiting.
//
// Control-plane commands (CmdCancelAgent, CmdInterruptAndSend,
// CmdShutdownServer) are the exception: they go through c.dispatchControl
// instead, which uses a separate, much larger semaphore. All three handlers
// are fast — they signal cancellation or request shutdown via in-memory
// channels/context, never AgentCoordinator.Run — and must not be able to
// queue behind maxConcurrentHandlersPerConn long-running turns
// (e.g. handleSendMessage), since that's exactly when a user most needs
// cancel/interrupt/shutdown to go through promptly. See dispatchControl's
// doc comment for the full story.
func handleIncoming(ctx context.Context, s *Server, c *Client, raw []byte) {
	a := s.app
	var msg WSMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		slog.Debug("ws: malformed message", "err", err)
		c.reply("", EventError, nil, "malformed message")
		return
	}

	switch msg.Type {
	case CmdSendMessage:
		c.dispatch("handleSendMessage", msg.ID, func() { handleSendMessage(ctx, a, c, msg) })
	case CmdInterruptAndSend:
		// Control-plane: must cancel an in-flight (possibly stuck/long-running)
		// turn promptly, so it cannot share sem with handleSendMessage — see
		// dispatchControl's doc comment in hub.go for the full rationale.
		c.dispatchControl("handleInterruptAndSend", msg.ID, func() { handleInterruptAndSend(ctx, a, c, msg) })
	case CmdInjectMessage:
		c.dispatch("handleInjectMessage", msg.ID, func() { handleInjectMessage(ctx, a, c, msg) })
	case CmdCancelAgent:
		// Control-plane: same rationale as CmdInterruptAndSend above.
		c.dispatchControl("handleCancelAgent", msg.ID, func() { handleCancelAgent(ctx, a, c, msg) })
	case CmdShutdownServer:
		// Control-plane: a shutdown request must not queue behind long-running
		// turns — same rationale as CmdInterruptAndSend above.
		c.dispatchControl("handleShutdownServer", msg.ID, func() { handleShutdownServer(s, c, msg) })
	case CmdCreateSession:
		c.dispatch("handleCreateSession", msg.ID, func() { handleCreateSession(ctx, a, c, msg) })
	case CmdForkSession:
		c.dispatch("handleForkSession", msg.ID, func() { handleForkSession(ctx, a, c, msg) })
	case CmdDeleteSession:
		c.dispatch("handleDeleteSession", msg.ID, func() { handleDeleteSession(ctx, a, c, msg) })
	case CmdDeleteOtherSessions:
		c.dispatch("handleDeleteOtherSessions", msg.ID, func() { handleDeleteOtherSessions(ctx, a, c, msg) })
	case CmdListSessions:
		c.dispatch("handleListSessions", msg.ID, func() { handleListSessions(ctx, a, c, msg) })
	case CmdLoadMessages:
		c.dispatch("handleLoadMessages", msg.ID, func() { handleLoadMessages(ctx, a, c, msg) })
	case CmdGetConfig:
		c.dispatch("handleGetConfig", msg.ID, func() { handleGetConfig(a, c, msg) })
	case CmdGetLogs:
		c.dispatch("handleGetLogs", msg.ID, func() { handleGetLogs(a, c, msg) })
	case CmdSetTheme:
		c.dispatch("handleSetTheme", msg.ID, func() { handleSetTheme(a, c, msg) })
	case CmdSetKeepAlive:
		c.dispatch("handleSetKeepAlive", msg.ID, func() { handleSetKeepAlive(a, c, msg) })
	case CmdRenameSession:
		c.dispatch("handleRenameSession", msg.ID, func() { handleRenameSession(ctx, a, c, msg) })
	case CmdSetSessionModels:
		c.dispatch("handleSetSessionModels", msg.ID, func() { handleSetSessionModels(ctx, a, c, msg) })
	case CmdRemoveRecentModel:
		c.dispatch("handleRemoveRecentModel", msg.ID, func() { handleRemoveRecentModel(a, c, msg) })
	case CmdTrackModelUsage:
		c.dispatch("handleTrackModelUsage", msg.ID, func() { handleTrackModelUsage(a, c, msg) })
	case CmdGetScopedModels:
		c.dispatch("handleGetScopedModels", msg.ID, func() { handleGetScopedModels(a, c, msg) })
	case CmdSetScopedModel:
		c.dispatch("handleSetScopedModel", msg.ID, func() { handleSetScopedModel(a, c, msg) })
	case CmdClearScopedModel:
		c.dispatch("handleClearScopedModel", msg.ID, func() { handleClearScopedModel(a, c, msg) })
	case CmdSetProviderKey:
		c.dispatch("handleSetProviderKey", msg.ID, func() { handleSetProviderKey(a, c, msg) })
	case CmdRemoveProviderKey:
		c.dispatch("handleRemoveProviderKey", msg.ID, func() { handleRemoveProviderKey(a, c, msg) })
	case CmdDeleteMessage:
		c.dispatch("handleDeleteMessage", msg.ID, func() { handleDeleteMessage(ctx, a, c, msg) })
	case CmdDeleteMessages:
		c.dispatch("handleDeleteMessages", msg.ID, func() { handleDeleteMessages(ctx, a, c, msg) })
	case CmdUpdateMessageContent:
		c.dispatch("handleUpdateMessageContent", msg.ID, func() { handleUpdateMessageContent(ctx, a, c, msg) })
	case CmdUpdateMessageThinking:
		c.dispatch("handleUpdateMessageThinking", msg.ID, func() { handleUpdateMessageThinking(ctx, a, c, msg) })
	case CmdGetSystemPrompt:
		c.dispatch("handleGetSystemPrompt", msg.ID, func() { handleGetSystemPrompt(ctx, a, c, msg) })
	case CmdSetSystemPrompt:
		c.dispatch("handleSetSystemPrompt", msg.ID, func() { handleSetSystemPrompt(ctx, a, c, msg) })
	case CmdSummarizeSession:
		c.dispatch("handleSummarizeSession", msg.ID, func() { handleSummarizeSession(ctx, a, c, msg) })
	case CmdCancelQueuedSummarize:
		c.dispatch("handleCancelQueuedSummarize", msg.ID, func() { handleCancelQueuedSummarize(a, c, msg) })
	case CmdDeleteMessagePart:
		c.dispatch("handleDeleteMessagePart", msg.ID, func() { handleDeleteMessagePart(ctx, a, c, msg) })
	case CmdUpdateMessagePart:
		c.dispatch("handleUpdateMessagePart", msg.ID, func() { handleUpdateMessagePart(ctx, a, c, msg) })
	case CmdTogglePinMessage:
		c.dispatch("handleTogglePinMessage", msg.ID, func() { handleTogglePinMessage(ctx, a, c, msg) })
	case CmdRerunMessage:
		c.dispatch("handleRerunMessage", msg.ID, func() { handleRerunMessage(ctx, a, c, msg) })
	case CmdLogClientEvent:
		c.dispatch("handleLogClientEvent", msg.ID, func() { handleLogClientEvent(a, c, msg) })
	case CmdLogClientError:
		c.dispatch("handleLogClientError", msg.ID, func() { handleLogClientError(c, msg) })
	case CmdSetMCPDisabled:
		c.dispatch("handleSetMCPDisabled", msg.ID, func() { handleSetMCPDisabled(ctx, a, c, msg) })
	case CmdAddMCPServer:
		c.dispatch("handleAddMCPServer", msg.ID, func() { handleAddMCPServer(ctx, a, c, msg) })
	case CmdRemoveMCPServer:
		c.dispatch("handleRemoveMCPServer", msg.ID, func() { handleRemoveMCPServer(a, c, msg) })
	case CmdUpdateMCPServer:
		c.dispatch("handleUpdateMCPServer", msg.ID, func() { handleUpdateMCPServer(ctx, a, c, msg) })
	case CmdSetDebug:
		c.dispatch("handleSetDebug", msg.ID, func() { handleSetDebug(a, c, msg) })
	case CmdAddContextPath:
		c.dispatch("handleAddContextPath", msg.ID, func() { handleAddContextPath(a, c, msg) })
	case CmdRemoveContextPath:
		c.dispatch("handleRemoveContextPath", msg.ID, func() { handleRemoveContextPath(a, c, msg) })
	case CmdGetSkills:
		c.dispatch("handleGetSkills", msg.ID, func() { handleGetSkills(a, c, msg) })
	case CmdAddSkillsPath:
		c.dispatch("handleAddSkillsPath", msg.ID, func() { handleAddSkillsPath(a, c, msg) })
	case CmdRemoveSkillsPath:
		c.dispatch("handleRemoveSkillsPath", msg.ID, func() { handleRemoveSkillsPath(a, c, msg) })
	case CmdInitializeProject:
		c.dispatch("handleInitializeProject", msg.ID, func() { handleInitializeProject(ctx, a, c, msg) })
	case CmdAddCustomProvider:
		c.dispatch("handleAddCustomProvider", msg.ID, func() { handleAddCustomProvider(a, c, msg) })
	case CmdRemoveCustomProvider:
		c.dispatch("handleRemoveCustomProvider", msg.ID, func() { handleRemoveCustomProvider(a, c, msg) })
	case CmdUpdateCustomProvider:
		c.dispatch("handleUpdateCustomProvider", msg.ID, func() { handleUpdateCustomProvider(a, c, msg) })
	case CmdSetProviderPeakHours:
		c.dispatch("handleSetProviderPeakHours", msg.ID, func() { handleSetProviderPeakHours(a, c, msg) })
	case CmdUpdateTodos:
		c.dispatch("handleUpdateTodos", msg.ID, func() { handleUpdateTodos(ctx, a, c, msg) })
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
	// Options is a pointer and this was the only handler dereferencing it
	// unguarded: handlers_config.go checks it in five places and config's own
	// cloneOptions treats nil as a legal input. Nothing reachable produces a
	// nil today — setDefaults (config/load.go) always installs an &Options{}
	// — so this closes an inconsistency rather than a live crash.
	if cfg == nil || cfg.Options == nil || !cfg.Options.Debug {
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
