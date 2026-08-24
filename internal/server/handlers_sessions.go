package server

// Session-lifecycle handlers: create, fork, delete, rename and list; the
// external-ownership lock annotation those replies carry; per-session
// system prompt get/set; and todo updates (todos live on the session row).

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/session"
)

func handleCreateSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p CreateSessionPayload
	if len(msg.Payload) > 0 && string(msg.Payload) != "null" {
		if err := json.Unmarshal(msg.Payload, &p); err != nil {
			c.reply(msg.ID, EventError, nil, "invalid payload")
			return
		}
	}
	title := p.Title
	if title == "" {
		title = "New Session"
	}
	sess, err := a.Sessions.Create(ctx, title)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Web sessions never prompt for permissions — arm auto-approve at birth.
	autoApproveWebSession(a, sess.ID)

	// Deliberately do NOT seed the new session's smart/fast model columns
	// from config here. A session is created with no override
	// (SmartModelID/FastModelID == "") so it INHERITS the system/folder
	// default and keeps following it if that default changes later —
	// resolveSessionModels (internal/agent/coordinator.go) already falls
	// back to cfg.Models on every call when the session has no override, and
	// BuildSystemPromptForSession below goes through that same resolution.
	// Writing the resolved default into the row at creation time used to
	// freeze it permanently: any later change to the folder/system default
	// would silently stop applying to that session, defeating the system ->
	// folder -> session cascade for every session that never explicitly
	// picked a model (task #461).

	// Generate and save the system prompt for the new session.
	if a.AgentCoordinator != nil {
		if sp, err := a.AgentCoordinator.BuildSystemPromptForSession(ctx, sess.ID); err == nil && sp != "" {
			if err := a.AgentCoordinator.UpdateSessionSystemPrompt(ctx, sess.ID, sp); err == nil {
				if updated, err := a.Sessions.Get(ctx, sess.ID); err == nil {
					sess = updated
				}
			}
		}
	}

	// Broadcast to all clients so every tab sees the new session.
	c.hub.Broadcast(EventSessionCreated, sess)
}

func handleForkSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p ForkSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "sessionID required")
		return
	}

	// ForkSession performs the clone atomically in one DB transaction: the
	// new session row plus every copied message commit together or not at
	// all, so a midway failure surfaces as an explicit error rather than a
	// half-built fork the client mistakes for a complete one.
	fork, err := a.Sessions.ForkSession(ctx, p.SessionID, p.Title)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Web sessions never prompt for permissions — arm auto-approve at birth.
	autoApproveWebSession(a, fork.ID)

	// Broadcast so all tabs see the fork and switch to it
	c.hub.Broadcast(EventSessionCreated, fork)
}

func handleDeleteSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Sessions.Delete(ctx, p.SessionID); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// handleDeleteOtherSessions deletes every top-level session except the one
// identified by KeepID. Sub-sessions are not deleted directly — they are
// cleaned up by a.Sessions.Delete when their parent is removed, mirroring
// handleDeleteSession. Each deletion publishes a DeletedEvent that the
// events.go pubsub bridge broadcasts as session_deleted, so every connected
// client updates. A no-op ack is returned when KeepID is empty or there is
// nothing else to delete.
func handleDeleteOtherSessions(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p DeleteOtherSessionsPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if p.KeepID == "" {
		c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
		return
	}
	sessions, err := a.Sessions.List(ctx)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	for _, s := range sessions {
		// Skip the kept session and any sub-session (those go when their
		// parent is deleted, matching handleDeleteSession's behaviour).
		if s.ID == p.KeepID || s.ParentSessionID != "" {
			continue
		}
		if err := a.Sessions.Delete(ctx, s.ID); err != nil {
			slog.Warn("delete_other_sessions: failed to delete session", "id", s.ID, "err", err)
		}
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// externalOwnerLiveThreshold mirrors the heartbeat expiry used by the lock
// acquisition path: a lock whose mtime is fresher than this is treated as a
// live external owner by the mtime fast path. The lock-renewer touches the
// file every ~10s, so 20s gives one missed tick of slack without flipping
// foreign-owned sessions in and out of read-only mode.
//
// This is no longer the whole story (task #228): InspectSessionLock (which
// both annotator functions below call) falls back to a real PID liveness
// probe when mtime looks stale past this threshold, so a session blocked on
// one long tool call — whose heartbeat mtime can lag past 20s, since it's
// gated on activity ticks from the stream watchdog which fire roughly every
// 30s (task #222) — is still correctly reported as live rather than
// flickering to "not externally owned" and back.
const externalOwnerLiveThreshold = 20 * time.Second

// annotateExternalOwnership fills OwnedExternal/OwnedByPID for every session
// in the slice. Only flags sessions whose live lock holder (mtime freshness,
// falling back to real PID liveness when mtime looks stale — see
// InspectSessionLock) is a DIFFERENT process — sessions held by us are
// owned-but-not-external (the UI keeps full controls). Sessions with no
// lock, or a lock that is both mtime-stale and PID-dead, are left clean.
func annotateExternalOwnership(a *appPkg.App, sessions []session.Session) {
	dataDir := externalOwnershipDataDir(a)
	if dataDir == "" {
		return
	}
	self := os.Getpid()
	for i := range sessions {
		st := session.InspectSessionLock(dataDir, sessions[i].ID, externalOwnerLiveThreshold)
		if !st.Live || st.PID == 0 || st.PID == self {
			continue
		}
		sessions[i].OwnedExternal = true
		sessions[i].OwnedByPID = st.PID
	}
}

// AnnotateSessionExternalOwnership is the single-session variant used by the
// session pubsub bridge in events.go and by every handler that broadcasts a
// fresh Session payload over WS. Exported so events.go can reach it without
// duplicating the lock-inspection logic. See annotateExternalOwnership's doc
// comment for the mtime-plus-PID-fallback liveness semantics (task #228).
func AnnotateSessionExternalOwnership(a *appPkg.App, s *session.Session) {
	if s == nil {
		return
	}
	dataDir := externalOwnershipDataDir(a)
	if dataDir == "" {
		return
	}
	self := os.Getpid()
	st := session.InspectSessionLock(dataDir, s.ID, externalOwnerLiveThreshold)
	if !st.Live || st.PID == 0 || st.PID == self {
		s.OwnedExternal = false
		s.OwnedByPID = 0
		return
	}
	s.OwnedExternal = true
	s.OwnedByPID = st.PID
}

func externalOwnershipDataDir(a *appPkg.App) string {
	cfg := a.Config()
	if cfg == nil || cfg.Options == nil {
		return ""
	}
	return cfg.Options.DataDirectory
}

func handleListSessions(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	sessions, err := a.Sessions.List(ctx)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	if sessions == nil {
		sessions = []session.Session{}
	}
	annotateExternalOwnership(a, sessions)
	c.reply(msg.ID, EventSessionsList, sessions, "")

	// Correct any stale agent_busy and summarize_queued state in the replay
	// buffer by sending the server's authoritative state for every session
	// to this client only (not broadcast — other clients already have accurate
	// live state).
	if a.AgentCoordinator != nil {
		for _, s := range sessions {
			busy := a.AgentCoordinator.IsSessionBusy(s.ID)
			queued := a.AgentCoordinator.SummarizeQueued(s.ID)
			c.reply("", EventAgentBusy, AgentBusyPayload{SessionID: s.ID, Busy: busy}, "")
			c.reply("", EventSummarizeQueued, SummarizeQueuedPayload{SessionID: s.ID, Queued: queued}, "")
		}
	}
}

func handleRenameSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p RenameSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if err := a.Sessions.Rename(ctx, p.SessionID, p.Title); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	sess, err := a.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// The rename broadcast is the only notification a rename produces (Rename
	// publishes no pubsub event), so it must carry the ownership annotation
	// like every other Session broadcast — see AnnotateSessionExternalOwnership.
	AnnotateSessionExternalOwnership(a, &sess)
	c.hub.Broadcast(EventSessionUpdated, sess)
}

func handleGetSystemPrompt(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p GetSystemPromptPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload: sessionID required")
		return
	}
	sess, err := a.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventSystemPrompt, map[string]string{"sessionID": p.SessionID, "content": sess.SystemPrompt}, "")
}

func handleSetSystemPrompt(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SetSystemPromptPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}
	if err := a.AgentCoordinator.UpdateSessionSystemPrompt(ctx, p.SessionID, p.Content); err != nil {
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Todos ─────────────────────────────────────────────────────────────────────

func handleUpdateTodos(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p UpdateTodosPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	sess, err := a.Sessions.Get(ctx, p.SessionID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, "session not found")
		return
	}
	todos := make([]session.Todo, len(p.Todos))
	for i, t := range p.Todos {
		todos[i] = session.Todo{
			Content:    t.Content,
			Status:     session.TodoStatus(t.Status),
			ActiveForm: t.ActiveForm,
		}
	}
	prev := sess.Todos
	sess.Todos = todos
	slog.Info(
		"ws: user updated todos",
		"session", p.SessionID,
		"prev_count", len(prev),
		"new_count", len(todos),
	)

	// Tombstone management: track which todos the operator explicitly removed.
	newByContent := make(map[string]struct{}, len(todos))
	for _, t := range todos {
		newByContent[t.Content] = struct{}{}
	}

	tombstones := append([]string(nil), sess.DeletedTodos...)
	tombstoneSet := make(map[string]struct{}, len(tombstones))
	for _, tc := range tombstones {
		tombstoneSet[tc] = struct{}{}
	}

	// Previous todos absent from the new list → add to tombstones.
	for _, t := range prev {
		if _, stillThere := newByContent[t.Content]; !stillThere {
			if _, already := tombstoneSet[t.Content]; !already {
				tombstones = append(tombstones, t.Content)
				tombstoneSet[t.Content] = struct{}{}
			}
		}
	}
	// Operator re-added a previously tombstoned todo → lift the tombstone.
	filtered := tombstones[:0]
	for _, content := range tombstones {
		if _, returned := newByContent[content]; !returned {
			filtered = append(filtered, content)
		}
	}
	sess.DeletedTodos = filtered

	if err := a.Sessions.SetTodos(ctx, sess.ID, todos, filtered); err != nil {
		c.reply(msg.ID, EventError, nil, "failed to save todos")
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
