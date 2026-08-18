package server

// Agent-turn handlers: send, interrupt-and-send, inject, cancel, rerun,
// summarize, and project initialization — everything that drives
// AgentCoordinator.Run — plus the attachment-saving helpers only these
// handlers use.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/charmbracelet/crush/internal/agent"
	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/google/uuid"
)

// saveAttachmentToDisk saves an attachment to <dataDir>/attachments/ with a
// timestamped filename and returns the absolute path. dataDir must already be
// the fully resolved data directory (e.g. cfg.Options.DataDirectory, which
// defaults to "<workingDir>/.crush" but honors an explicit --data-dir or
// configured data_directory) — callers must not append ".crush" themselves.
func saveAttachmentToDisk(dataDir, fileName string, data []byte) (string, error) {
	if dataDir == "" {
		return "", errors.New("data directory not configured")
	}
	dir := filepath.Join(dataDir, "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create attachments dir: %w", err)
	}
	ts := time.Now().Format("2006-01-02_15-04-05")
	// A uuid suffix, not just the second-precision timestamp, makes a
	// filename collision between two same-named attachments uploaded
	// within the same second astronomically unlikely (32 bits of entropy
	// per upload) rather than a near-certainty (task #274) -- os.WriteFile
	// below would otherwise silently let the second upload overwrite the
	// first's content. A uuid, not #275's atomic counter, on purpose: an
	// atomic counter is only unique WITHIN one process, but multiple crush
	// processes can share this same dataDir/attachments directory.
	name := ts + "_" + uuid.NewString()[:8] + "_" + filepath.Base(fileName)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write attachment: %w", err)
	}
	return path, nil
}

func handleSendMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SendMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleSendMessage", "sessionID", p.SessionID, "content", p.Content, "attachments", len(p.Attachments))

	// Save attachments to disk and append file paths to the prompt text.
	// This ensures CLI-based agents can access files via their read tools.
	var attachments []message.Attachment
	for _, att := range p.Attachments {
		slog.Info("ws: attachment received", "fileName", att.FileName, "mimeType", att.MimeType, "dataLen", len(att.Data))

		// Save to <data dir>/attachments/ with timestamped name.
		savedPath, saveErr := saveAttachmentToDisk(attachmentsDataDir(a), att.FileName, att.Data)
		if saveErr != nil {
			slog.Warn("ws: failed to save attachment to disk", "err", saveErr)
		} else {
			p.Content += "\n[Attached file: " + savedPath + "]"
			slog.Info("ws: attachment saved", "path", savedPath)
		}

		attachments = append(attachments, message.Attachment{
			FileName: att.FileName,
			MimeType: att.MimeType,
			Content:  att.Data,
		})
	}

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// A human re-entering the loop re-arms Phase 4 autonomy for this session.
	autoApproveWebSession(a, p.SessionID)
	a.AgentCoordinator.ResetAutoResumeCounter(p.SessionID)

	// Priority:
	// 1. Explicit override in message payload (from UI)
	// 2. Models stored in the session record in DB
	// 3. Global defaults from config

	var largeOverride, smallOverride *agent.ModelOverride

	// Check payload first
	if p.LargeModel != nil {
		largeOverride = &agent.ModelOverride{Provider: p.LargeModel.Provider, Model: p.LargeModel.Model}
	}
	if p.SmallModel != nil {
		smallOverride = &agent.ModelOverride{Provider: p.SmallModel.Provider, Model: p.SmallModel.Model}
	}

	// If no payload override, check DB
	if largeOverride == nil || smallOverride == nil {
		sess, err := a.Sessions.Get(ctx, p.SessionID)
		if err == nil {
			if largeOverride == nil && sess.LargeModelID != "" {
				slog.Info("ws: using models from DB", "sessionID", p.SessionID, "large", sess.LargeModelID)
				largeOverride = &agent.ModelOverride{Provider: sess.LargeModelProvider, Model: sess.LargeModelID}
			}
			if smallOverride == nil && sess.SmallModelID != "" {
				smallOverride = &agent.ModelOverride{Provider: sess.SmallModelProvider, Model: sess.SmallModelID}
			}
		}
	}

	if largeOverride != nil {
		slog.Info("ws: final models for run", "sessionID", p.SessionID, "large", largeOverride.Model)
	}

	// Decouple the agent run from the WebSocket connection lifetime.
	// Without this, closing/refreshing the browser tab would cancel the agent.
	// Explicit cancellation is still available via Cancel(sessionID).
	agentCtx := context.WithoutCancel(ctx)

	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: true})
	var err error
	if largeOverride != nil || smallOverride != nil {
		_, err = a.AgentCoordinator.RunWithOverrides(agentCtx, p.SessionID, p.Content, largeOverride, smallOverride, attachments...)
	} else {
		_, err = a.AgentCoordinator.Run(agentCtx, p.SessionID, p.Content, attachments...)
	}
	// P2-2 fix: broadcast the actual busy state derived from mailbox ownership,
	// not from this request handler's lifetime. Run() may have returned early
	// because the session was already owned by another turn (the call was queued),
	// but the original owner is still active. IsSessionBusy reflects the live
	// mailbox state and is the authoritative source of truth.
	if !a.AgentCoordinator.IsSessionBusy(p.SessionID) {
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})
	}

	if err != nil {
		slog.Error("ws: agent run error", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
	}

	// P2-1 fix: summarizeQueue is now drained by abandonOwnershipWithHandoff
	// when the session becomes idle, not by this web handler. This ensures
	// that pending summarise requests execute even when ownership transitions
	// via non-web paths (CLI, detached runs, etc.). The ownership transition
	// in abandonOwnershipWithHandoff is the authoritative drain point.
}

// handleInterruptAndSend cancels the running turn and queues a new user
// message in one shot. The in-flight agent.Run() finalises the cancelled
// assistant message with FinishReasonCanceled, then its cancel-handling
// branch drains the queue and immediately re-enters Run() with the new
// message — so the user keeps everything produced so far plus their new
// instruction.
func handleInterruptAndSend(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SendMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleInterruptAndSend", "sessionID", p.SessionID, "content", p.Content, "attachments", len(p.Attachments))

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// A human re-entering the loop re-arms Phase 4 autonomy for this session.
	autoApproveWebSession(a, p.SessionID)
	a.AgentCoordinator.ResetAutoResumeCounter(p.SessionID)

	// Same attachments path as handleSendMessage: save to disk, append paths
	// to the prompt text so CLI tools can read them, and forward attachment
	// metadata so vision-capable providers can ingest images.
	var attachments []message.Attachment
	for _, att := range p.Attachments {
		savedPath, saveErr := saveAttachmentToDisk(attachmentsDataDir(a), att.FileName, att.Data)
		if saveErr != nil {
			slog.Warn("ws: failed to save attachment to disk", "err", saveErr)
		} else {
			p.Content += "\n[Attached file: " + savedPath + "]"
		}
		attachments = append(attachments, message.Attachment{
			FileName: att.FileName,
			MimeType: att.MimeType,
			Content:  att.Data,
		})
	}

	// Model overrides follow the same priority as handleSendMessage:
	// payload > DB session record > global defaults.
	var largeOverride, smallOverride *agent.ModelOverride
	if p.LargeModel != nil {
		largeOverride = &agent.ModelOverride{Provider: p.LargeModel.Provider, Model: p.LargeModel.Model}
	}
	if p.SmallModel != nil {
		smallOverride = &agent.ModelOverride{Provider: p.SmallModel.Provider, Model: p.SmallModel.Model}
	}
	if largeOverride == nil || smallOverride == nil {
		if sess, err := a.Sessions.Get(ctx, p.SessionID); err == nil {
			if largeOverride == nil && sess.LargeModelID != "" {
				largeOverride = &agent.ModelOverride{Provider: sess.LargeModelProvider, Model: sess.LargeModelID}
			}
			if smallOverride == nil && sess.SmallModelID != "" {
				smallOverride = &agent.ModelOverride{Provider: sess.SmallModelProvider, Model: sess.SmallModelID}
			}
		}
	}

	// Use bounded context for idle-interrupt: WithoutCancel + timeout to ensure
	// the operation can complete even if the WebSocket connection closes, but with
	// a reasonable upper bound to prevent indefinite hangs (e.g., blocked SQLite write).
	agentCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if err := a.AgentCoordinator.InterruptAndSend(agentCtx, p.SessionID, p.Content, largeOverride, smallOverride, attachments...); err != nil {
		slog.Error("ws: interrupt-and-send failed", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	// Don't toggle EventAgentBusy here: the running handleSendMessage
	// goroutine will publish busy=false when its Run() returns, and the
	// queue drain inside Run() will publish busy=true again for the new
	// turn. Touching the flag here would create a flicker.
	c.reply(msg.ID, EventResponse, map[string]string{"status": "queued"}, "")
}

// handleInjectMessage persists a user message to the session DB right now
// (so the UI shows it instantly) and — if the session is busy — schedules
// the same message to be merged into the next provider request without
// cancelling the in-flight turn. See SessionAgent.InjectMessage for the
// drain-at-PrepareStep mechanism.
func handleInjectMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SendMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	slog.Info("ws: handleInjectMessage", "sessionID", p.SessionID, "content", p.Content, "attachments", len(p.Attachments))

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// A human re-entering the loop re-arms Phase 4 autonomy for this session.
	autoApproveWebSession(a, p.SessionID)
	a.AgentCoordinator.ResetAutoResumeCounter(p.SessionID)

	// Same attachments path as handleSendMessage.
	var attachments []message.Attachment
	for _, att := range p.Attachments {
		savedPath, saveErr := saveAttachmentToDisk(attachmentsDataDir(a), att.FileName, att.Data)
		if saveErr != nil {
			slog.Warn("ws: failed to save attachment to disk", "err", saveErr)
		} else {
			p.Content += "\n[Attached file: " + savedPath + "]"
		}
		attachments = append(attachments, message.Attachment{
			FileName: att.FileName,
			MimeType: att.MimeType,
			Content:  att.Data,
		})
	}

	agentCtx := context.WithoutCancel(ctx)
	if _, err := a.AgentCoordinator.InjectMessage(agentCtx, p.SessionID, p.Content, attachments...); err != nil {
		slog.Error("ws: inject-message failed", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "injected"}, "")
}

func handleCancelAgent(_ context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p CancelAgentPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	a.AgentCoordinator.Cancel(p.SessionID)
	// Force-broadcast busy=false immediately so the UI unblocks and the replay
	// buffer records a definitive "not busy" state. The goroutine will also
	// broadcast false when it actually finishes (harmless duplicate).
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})
}

// attachmentsDataDir resolves the configured data directory for saved
// attachments. It defensively falls back to "<workingDir>/.crush" (the
// pre-fix, cwd-derived default) on the rare nil-config edge case, so a
// missing config doesn't turn a best-effort attachment save into a hard
// failure.
func attachmentsDataDir(a *appPkg.App) string {
	return cmp.Or(externalOwnershipDataDir(a), filepath.Join(a.Store().WorkingDir(), ".crush"))
}

func handleSummarizeSession(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p SummarizeSessionPayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}
	agentCtx := context.WithoutCancel(ctx)
	// Summarize will queue the request and return ErrSummarizeQueued if busy.
	// We pass nil for the snapshot, which causes Summarize to resolve it from the target session.
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: true})
	err := a.AgentCoordinator.Summarize(agentCtx, p.SessionID, nil)
	if errors.Is(err, agent.ErrSummarizeQueued) {
		// The session is still busy with the owning turn that triggered the queue.
		// Do NOT broadcast Busy: false — that would mislead clients into thinking
		// the session is idle when it's still owned by the compaction turn.
		c.hub.Broadcast(EventSummarizeQueued, SummarizeQueuedPayload{SessionID: p.SessionID, Queued: true})
		c.reply(msg.ID, EventResponse, map[string]string{"status": "queued"}, "")
		return
	}
	// Broadcast the actual busy state derived from mailbox ownership.
	if !a.AgentCoordinator.IsSessionBusy(p.SessionID) {
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: false})
	}
	if err != nil {
		slog.Error("ws: summarize error", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

func handleCancelQueuedSummarize(a *appPkg.App, c *Client, msg WSMessage) {
	var p CancelQueuedSummarizePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil || p.SessionID == "" {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}
	a.AgentCoordinator.CancelQueuedSummarize(p.SessionID)
	c.hub.Broadcast(EventSummarizeQueued, SummarizeQueuedPayload{SessionID: p.SessionID, Queued: false})
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// ── Project initialization ────────────────────────────────────────────────────

func handleInitializeProject(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	store := a.Store()
	if store == nil {
		c.reply(msg.ID, EventError, nil, "config not available")
		return
	}
	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	initPrompt, err := agent.InitializePrompt(store)
	if err != nil {
		c.reply(msg.ID, EventError, nil, "failed to build initialization prompt: "+err.Error())
		return
	}

	// Create a dedicated initialization session.
	sess, err := a.Sessions.Create(ctx, "Project Initialization")
	if err != nil {
		c.reply(msg.ID, EventError, nil, "failed to create session: "+err.Error())
		return
	}

	// No explicit model seeding here either — see the identical comment in
	// handleCreateSession. This session inherits the system/folder default
	// via resolveSessionModels, same as any other freshly created session.

	// Build and save the system prompt.
	if sp, buildErr := a.AgentCoordinator.BuildSystemPromptForSession(ctx, sess.ID); buildErr == nil && sp != "" {
		_ = a.AgentCoordinator.UpdateSessionSystemPrompt(ctx, sess.ID, sp)
	}

	// Broadcast the new session before replying so the client can navigate.
	if updated, fetchErr := a.Sessions.Get(ctx, sess.ID); fetchErr == nil {
		c.hub.Broadcast(EventSessionCreated, updated)
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok", "sessionID": sess.ID}, "")

	// Run the agent in a background context so closing the tab won't cancel it.
	agentCtx := context.WithoutCancel(ctx)
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sess.ID, Busy: true})
	_, runErr := a.AgentCoordinator.Run(agentCtx, sess.ID, initPrompt)
	// P2-2 fix: broadcast the actual busy state derived from mailbox ownership.
	if !a.AgentCoordinator.IsSessionBusy(sess.ID) {
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sess.ID, Busy: false})
	}
	if runErr != nil {
		slog.Error("ws: initialization run error", "err", runErr)
	}
	_ = config.MarkProjectInitialized(a.Store())
}

// handleRerunMessage is an atomic "retry from this user message": it cancels
// any in-flight agent run, waits for idle, deletes every message created AFTER
// the target user message, then deletes the target itself and re-runs the agent
// with the same prompt. Run() creates a fresh user message so the history reads
// naturally. All steps happen in one goroutine — no client-side race.
func handleRerunMessage(ctx context.Context, a *appPkg.App, c *Client, msg WSMessage) {
	var p RerunMessagePayload
	if err := json.Unmarshal(msg.Payload, &p); err != nil {
		c.reply(msg.ID, EventError, nil, "invalid payload")
		return
	}

	targetMsg, err := a.Messages.Get(ctx, p.MessageID)
	if err != nil {
		c.reply(msg.ID, EventError, nil, "message not found")
		return
	}
	if targetMsg.Role != message.User {
		c.reply(msg.ID, EventError, nil, "can only rerun user messages")
		return
	}

	text := targetMsg.Content().Text
	if text == "" {
		c.reply(msg.ID, EventError, nil, "empty message")
		return
	}

	sessionID := targetMsg.SessionID
	slog.Info("ws: handleRerunMessage", "sessionID", sessionID, "messageID", p.MessageID,
		"contentPreview", text[:min(len(text), 80)])

	if a.AgentCoordinator == nil {
		c.reply(msg.ID, EventError, nil, "agent not configured")
		return
	}

	// Web sessions never prompt for permissions.
	autoApproveWebSession(a, sessionID)

	// 1. Cancel + clear queue if busy, then poll until idle (up to 10s).
	a.AgentCoordinator.Cancel(sessionID)
	a.AgentCoordinator.ClearQueue(sessionID)
	idle := false
	for i := 0; i < 100; i++ {
		if !a.AgentCoordinator.IsSessionBusy(sessionID) {
			idle = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// P1-6 fix: fail closed if the session is still busy after the timeout.
	// Provider/tool can legitimately respond to cancellation longer than 10s,
	// and the old owner may still be writing to history. Proceeding would race
	// between deletion and concurrent writes, corrupting the transcript.
	if !idle {
		slog.Warn("ws: rerun: session still stopping after timeout",
			"sessionID", sessionID,
			"messageID", p.MessageID,
			"timeout_seconds", 10)
		c.reply(msg.ID, EventError, nil, "session still stopping — please retry")
		return
	}

	// 2. Delete every message AFTER the target (by CreatedAt), keep the target.
	allMsgs, listErr := a.Messages.List(ctx, sessionID)
	if listErr != nil {
		c.reply(msg.ID, EventError, nil, "failed to list messages")
		return
	}
	for _, m := range allMsgs {
		if m.CreatedAt > targetMsg.CreatedAt ||
			(m.CreatedAt == targetMsg.CreatedAt && m.ID != targetMsg.ID) {
			if delErr := a.Messages.Delete(ctx, m.ID); delErr != nil {
				slog.Warn("ws: rerun: failed to delete tail message", "id", m.ID, "err", delErr)
			}
		}
	}

	// 3. Delete the original user message — Run() will recreate it.
	if delErr := a.Messages.Delete(ctx, targetMsg.ID); delErr != nil {
		slog.Warn("ws: rerun: failed to delete original user message", "id", targetMsg.ID, "err", delErr)
	}

	// 4. Re-arm Phase 4 autonomy.
	a.AgentCoordinator.ResetAutoResumeCounter(sessionID)

	// 5. Resolve model overrides (same priority as handleSendMessage).
	var largeOverride, smallOverride *agent.ModelOverride
	if sess, sessErr := a.Sessions.Get(ctx, sessionID); sessErr == nil {
		if sess.LargeModelID != "" {
			largeOverride = &agent.ModelOverride{Provider: sess.LargeModelProvider, Model: sess.LargeModelID}
		}
		if sess.SmallModelID != "" {
			smallOverride = &agent.ModelOverride{Provider: sess.SmallModelProvider, Model: sess.SmallModelID}
		}
	}

	// 6. Run the agent with the same prompt.
	agentCtx := context.WithoutCancel(ctx)
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sessionID, Busy: true})
	if largeOverride != nil || smallOverride != nil {
		_, err = a.AgentCoordinator.RunWithOverrides(agentCtx, sessionID, text, largeOverride, smallOverride)
	} else {
		_, err = a.AgentCoordinator.Run(agentCtx, sessionID, text)
	}
	// P2-2 fix: broadcast the actual busy state derived from mailbox ownership,
	// not from this request handler's lifetime.
	if !a.AgentCoordinator.IsSessionBusy(sessionID) {
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sessionID, Busy: false})
	}

	if err != nil {
		slog.Error("ws: rerun agent error", "err", err)
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}
