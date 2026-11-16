package server

import (
	"context"
	"encoding/json"
	"log/slog"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsapi"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsconfig"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wswork"
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
	if wsapi.Dispatch(ctx, a, c, msg) {
		return
	}
	if wswork.Dispatch(ctx, a, c, msg, wsworkSeams()) {
		return
	}
	if wsconfig.Dispatch(ctx, a, c, msg) {
		return
	}

	switch msg.Type {
	case CmdLogClientEvent:
		c.Dispatch("handleLogClientEvent", msg.ID, func() { handleLogClientEvent(a, c, msg) })
	case CmdLogClientError:
		c.Dispatch("handleLogClientError", msg.ID, func() { handleLogClientError(c, msg) })
	case CmdShutdownServer:
		// Control-plane shutdown stays in the root package.
		c.DispatchControl("handleShutdownServer", msg.ID, func() { handleShutdownServer(s, c, msg) })
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
