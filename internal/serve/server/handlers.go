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
		c.Reply("", EventError, nil, "malformed message")
		return
	}

	// Sprint 0 L4 (S4, design §3): the authorization block moved verbatim
	// into Client.AuthorizeIncoming (hub/scope.go); it replies with the same
	// denial envelopes and returns non-zero when the frame is denied.
	if status := c.AuthorizeIncoming(msg.ID, msg.Type, msg.Payload); status != 0 {
		return
	}

	switch msg.Type {
	case CmdSendMessage:
		c.Dispatch("handleSendMessage", msg.ID, func() { handleSendMessage(ctx, a, c, msg) })
	case CmdInterruptAndSend:
		// Control-plane: must cancel an in-flight (possibly stuck/long-running)
		// turn promptly, so it cannot share sem with handleSendMessage — see
		// dispatchControl's doc comment in hub.go for the full rationale.
		c.DispatchControl("handleInterruptAndSend", msg.ID, func() { handleInterruptAndSend(ctx, a, c, msg) })
	case CmdInjectMessage:
		c.Dispatch("handleInjectMessage", msg.ID, func() { handleInjectMessage(ctx, a, c, msg) })
	case CmdCancelAgent:
		// Control-plane: same rationale as CmdInterruptAndSend above.
		c.DispatchControl("handleCancelAgent", msg.ID, func() { handleCancelAgent(ctx, a, c, msg) })
	case CmdShutdownServer:
		// Control-plane: a shutdown request must not queue behind long-running
		// turns — same rationale as CmdInterruptAndSend above.
		c.DispatchControl("handleShutdownServer", msg.ID, func() { handleShutdownServer(s, c, msg) })
	case CmdCreateSession:
		c.Dispatch("handleCreateSession", msg.ID, func() { handleCreateSession(ctx, a, c, msg) })
	case CmdForkSession:
		c.Dispatch("handleForkSession", msg.ID, func() { handleForkSession(ctx, a, c, msg) })
	case CmdDeleteSession:
		c.Dispatch("handleDeleteSession", msg.ID, func() { handleDeleteSession(ctx, a, c, msg) })
	case CmdDeleteOtherSessions:
		c.Dispatch("handleDeleteOtherSessions", msg.ID, func() { handleDeleteOtherSessions(ctx, a, c, msg) })
	case CmdListSessions:
		c.Dispatch("handleListSessions", msg.ID, func() { handleListSessions(ctx, a, c, msg) })
	case CmdLoadMessages:
		c.Dispatch("handleLoadMessages", msg.ID, func() { handleLoadMessages(ctx, a, c, msg) })
	case CmdGetConfig:
		c.Dispatch("handleGetConfig", msg.ID, func() { handleGetConfig(a, c, msg) })
	case CmdGetLogs:
		c.Dispatch("handleGetLogs", msg.ID, func() { handleGetLogs(a, c, msg) })
	case CmdSetTheme:
		c.Dispatch("handleSetTheme", msg.ID, func() { handleSetTheme(a, c, msg) })
	case CmdSetKeepAlive:
		c.Dispatch("handleSetKeepAlive", msg.ID, func() { handleSetKeepAlive(a, c, msg) })
	case CmdRenameSession:
		c.Dispatch("handleRenameSession", msg.ID, func() { handleRenameSession(ctx, a, c, msg) })
	case CmdSetSessionModels:
		c.Dispatch("handleSetSessionModels", msg.ID, func() { handleSetSessionModels(ctx, a, c, msg) })
	case CmdRemoveRecentModel:
		c.Dispatch("handleRemoveRecentModel", msg.ID, func() { handleRemoveRecentModel(a, c, msg) })
	case CmdTrackModelUsage:
		c.Dispatch("handleTrackModelUsage", msg.ID, func() { handleTrackModelUsage(a, c, msg) })
	case CmdGetScopedModels:
		c.Dispatch("handleGetScopedModels", msg.ID, func() { handleGetScopedModels(a, c, msg) })
	case CmdSetScopedModel:
		c.Dispatch("handleSetScopedModel", msg.ID, func() { handleSetScopedModel(a, c, msg) })
	case CmdClearScopedModel:
		c.Dispatch("handleClearScopedModel", msg.ID, func() { handleClearScopedModel(a, c, msg) })
	case CmdGetCustomRoles:
		c.Dispatch("handleGetCustomRoles", msg.ID, func() { handleGetCustomRoles(a, c, msg) })
	case CmdSetCustomRole:
		c.Dispatch("handleSetCustomRole", msg.ID, func() { handleSetCustomRole(a, c, msg) })
	case CmdRemoveCustomRole:
		c.Dispatch("handleRemoveCustomRole", msg.ID, func() { handleRemoveCustomRole(a, c, msg) })
	case CmdSetProviderKey:
		c.Dispatch("handleSetProviderKey", msg.ID, func() { handleSetProviderKey(a, c, msg) })
	case CmdRemoveProviderKey:
		c.Dispatch("handleRemoveProviderKey", msg.ID, func() { handleRemoveProviderKey(a, c, msg) })
	case CmdDeleteMessage:
		c.Dispatch("handleDeleteMessage", msg.ID, func() { handleDeleteMessage(ctx, a, c, msg) })
	case CmdDeleteMessages:
		c.Dispatch("handleDeleteMessages", msg.ID, func() { handleDeleteMessages(ctx, a, c, msg) })
	case CmdUpdateMessageContent:
		c.Dispatch("handleUpdateMessageContent", msg.ID, func() { handleUpdateMessageContent(ctx, a, c, msg) })
	case CmdUpdateMessageThinking:
		c.Dispatch("handleUpdateMessageThinking", msg.ID, func() { handleUpdateMessageThinking(ctx, a, c, msg) })
	case CmdGetSystemPrompt:
		c.Dispatch("handleGetSystemPrompt", msg.ID, func() { handleGetSystemPrompt(ctx, a, c, msg) })
	case CmdSetSystemPrompt:
		c.Dispatch("handleSetSystemPrompt", msg.ID, func() { handleSetSystemPrompt(ctx, a, c, msg) })
	case CmdSummarizeSession:
		c.Dispatch("handleSummarizeSession", msg.ID, func() { handleSummarizeSession(ctx, a, c, msg) })
	case CmdCancelQueuedSummarize:
		c.Dispatch("handleCancelQueuedSummarize", msg.ID, func() { handleCancelQueuedSummarize(a, c, msg) })
	case CmdDeleteMessagePart:
		c.Dispatch("handleDeleteMessagePart", msg.ID, func() { handleDeleteMessagePart(ctx, a, c, msg) })
	case CmdUpdateMessagePart:
		c.Dispatch("handleUpdateMessagePart", msg.ID, func() { handleUpdateMessagePart(ctx, a, c, msg) })
	case CmdTogglePinMessage:
		c.Dispatch("handleTogglePinMessage", msg.ID, func() { handleTogglePinMessage(ctx, a, c, msg) })
	case CmdRerunMessage:
		c.Dispatch("handleRerunMessage", msg.ID, func() { handleRerunMessage(ctx, a, c, msg) })
	case CmdLogClientEvent:
		c.Dispatch("handleLogClientEvent", msg.ID, func() { handleLogClientEvent(a, c, msg) })
	case CmdLogClientError:
		c.Dispatch("handleLogClientError", msg.ID, func() { handleLogClientError(c, msg) })
	case CmdSetMCPDisabled:
		c.Dispatch("handleSetMCPDisabled", msg.ID, func() { handleSetMCPDisabled(ctx, a, c, msg) })
	case CmdAddMCPServer:
		c.Dispatch("handleAddMCPServer", msg.ID, func() { handleAddMCPServer(ctx, a, c, msg) })
	case CmdRemoveMCPServer:
		c.Dispatch("handleRemoveMCPServer", msg.ID, func() { handleRemoveMCPServer(a, c, msg) })
	case CmdUpdateMCPServer:
		c.Dispatch("handleUpdateMCPServer", msg.ID, func() { handleUpdateMCPServer(ctx, a, c, msg) })
	case CmdSetDebug:
		c.Dispatch("handleSetDebug", msg.ID, func() { handleSetDebug(a, c, msg) })
	case CmdAddContextPath:
		c.Dispatch("handleAddContextPath", msg.ID, func() { handleAddContextPath(a, c, msg) })
	case CmdRemoveContextPath:
		c.Dispatch("handleRemoveContextPath", msg.ID, func() { handleRemoveContextPath(a, c, msg) })
	case CmdGetSkills:
		c.Dispatch("handleGetSkills", msg.ID, func() { handleGetSkills(a, c, msg) })
	case CmdAddSkillsPath:
		c.Dispatch("handleAddSkillsPath", msg.ID, func() { handleAddSkillsPath(a, c, msg) })
	case CmdRemoveSkillsPath:
		c.Dispatch("handleRemoveSkillsPath", msg.ID, func() { handleRemoveSkillsPath(a, c, msg) })
	case CmdInitializeProject:
		c.Dispatch("handleInitializeProject", msg.ID, func() { handleInitializeProject(ctx, a, c, msg) })
	case CmdAddCustomProvider:
		c.Dispatch("handleAddCustomProvider", msg.ID, func() { handleAddCustomProvider(a, c, msg) })
	case CmdRemoveCustomProvider:
		c.Dispatch("handleRemoveCustomProvider", msg.ID, func() { handleRemoveCustomProvider(a, c, msg) })
	case CmdUpdateCustomProvider:
		c.Dispatch("handleUpdateCustomProvider", msg.ID, func() { handleUpdateCustomProvider(a, c, msg) })
	case CmdSetProviderPeakHours:
		c.Dispatch("handleSetProviderPeakHours", msg.ID, func() { handleSetProviderPeakHours(a, c, msg) })
	case CmdPauseProviderPeakHours:
		c.Dispatch("handlePauseProviderPeakHours", msg.ID, func() { handlePauseProviderPeakHours(a, c, msg) })
	case CmdResumeProviderPeakHours:
		c.Dispatch("handleResumeProviderPeakHours", msg.ID, func() { handleResumeProviderPeakHours(a, c, msg) })
	case CmdGetPeakHoursStatus:
		c.Dispatch("handleGetPeakHoursStatus", msg.ID, func() { handleGetPeakHoursStatus(a, c, msg) })
	case CmdUpdateTodos:
		c.Dispatch("handleUpdateTodos", msg.ID, func() { handleUpdateTodos(ctx, a, c, msg) })
	case CmdGetSessionLiveWork:
		c.Dispatch("handleGetSessionLiveWork", msg.ID, func() { handleGetSessionLiveWork(ctx, a, c, msg) })
	case CmdGetSessionBatchWork:
		c.Dispatch("handleGetSessionBatchWork", msg.ID, func() { handleGetSessionBatchWork(ctx, a, c, msg) })
	case CmdBatchStatus, CmdBatchOutput:
		c.Dispatch("handleBatchRead", msg.ID, func() { handleBatchControl(ctx, a, c, msg) })
	case CmdBatchStop, CmdBatchAdd, CmdBatchDrain:
		c.DispatchControl("handleBatchControl", msg.ID, func() { handleBatchControl(ctx, a, c, msg) })
	case CmdGetSessionWakeSchedules:
		c.Dispatch("handleGetSessionWakeSchedules", msg.ID, func() { handleGetSessionWakeSchedules(ctx, a, c, msg) })
	case CmdCancelWakeSchedule:
		c.Dispatch("handleCancelWakeSchedule", msg.ID, func() { handleCancelWakeSchedule(ctx, a, c, msg) })
	default:
		slog.Debug("ws: unknown command", "type", msg.Type)
		c.Reply(msg.ID, EventError, nil, "unknown command: "+msg.Type)
	}
}

// autoApproveWebSession marks a session for blanket permission auto-approval.
// In the web UI there is no permission dialog — the agent must never block
// waiting for a user to grant/deny a tool call. This mirrors the
// non-interactive `rush run` path (app.RunNonInteractive →
// Permissions.AutoApproveSession). It is idempotent: re-arming an
// already-approved session is a no-op. The restricted-run allowlist is NOT
// armed here (unlike `rush run`), so approval is unconditional.
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
