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
	"github.com/charmbracelet/crush/internal/session"
	"github.com/google/uuid"
)

// rerunPostIdlePollSeam is a test-only hook (task #614): see its call site in
// handleRerunMessage for exactly which window it fires in. nil in every
// production path.
var rerunPostIdlePollSeam func()

// rerunHoldingReservationSeam is a test-only hook (task #614, reverse
// direction): see its call site in handleRerunMessage for exactly which
// window it fires in. nil in every production path.
var rerunHoldingReservationSeam func()

// rerunPreHandoffSeam is a test-only hook (task #614 F-2): fires right
// before Broadcast in handleRerunMessage's step 6, i.e. before
// RunWithReservedOwnership is called. Used to test that panics before
// the onHandoff callback still release the reservation. nil in every
// production path.
var rerunPreHandoffSeam func()

// externalSessionOwnerRefusal decides whether a history-destructive handler
// (handleRerunMessage's tail delete, deleteMessageRescuingOrphan's
// force-delete) may proceed for sessionID, and returns refuse=false only
// when no live OS-level session lock exists — or the only live one is
// provably this process's own. Any other outcome returns refuse=true with
// an operator-facing message. Task #622 (F-1 of the 2026-08-20 ninth
// review): the coordinator's mailbox can only prove in-process ownership;
// a `crush run --session S` in a DIFFERENT process is invisible to
// ReserveExclusive/IsSessionBusy, so any handler about to force-delete a
// still-streaming row must ALSO prove the absence of an OS-level owner
// first.
//
// This is deliberately NOT a mirror of annotateExternalOwnership, even
// though both call InspectSessionLock with the same threshold: that is a
// DISPLAY predicate — when it cannot identify an owner it leaves the
// session unflagged and the worst outcome is a UI showing full controls.
// This is a DESTRUCTION predicate — when it cannot identify an owner, the
// worst outcome is force-deleting a row a live external writer is still
// streaming into. Identical evidence, inverted safe default. The two
// therefore CAN disagree: a session with a live lock but unreadable PID
// shows as unowned in the session list yet refuses rerun/delete here.
//
// Three fail-closed rules follow from that inversion:
//
//  1. Live lock + PID 0 means "owner alive, identity unreadable", not
//     "nobody". On Windows (the dev platform) the lock file itself is
//     unreadable for the holder's whole lifetime (mandatory LockFileEx,
//     see readLockFile's doc in internal/session/lock.go), and the .pid
//     sidecar that compensates is written best-effort — writePIDSidecar
//     logs and swallows failures. The sidecar's "PID shown as 0 to a
//     concurrent reader" promise was scoped to diagnosability; wired into
//     a destructive decision, PID 0 must refuse. It could in principle be
//     OUR PID rendered unreadable, but the caller arrives here holding the
//     exclusive reservation with the session polled idle, so no in-process
//     turn is running and a live fresh-heartbeat lock is not expected to
//     be ours — and if it somehow is, the cost is one spurious "retry",
//     not lost data.
//  2. A data directory that cannot be resolved (nil store/config/options
//     or empty DataDirectory) refuses outright. attachmentsDataDir paper
//     over this edge with a workingDir fallback because a wrong guess only
//     misplaces files; here a wrong guess inspects the wrong locks/
//     directory, i.e. fails open. "Could not look" must not read as
//     "looked and found nothing".
//  3. The nil-store case is included in rule 2 rather than special-cased:
//     if it is unreachable in production the fail-closed branch costs
//     nothing, and if it is reachable it is exactly rule 2.
//
// Residual window, deliberately left open and distinct from the rules
// above: this is a check, not a claim — a process that acquires the OS
// lock a moment AFTER this inspection is still invisible until it surfaces
// some other way. Fully closing that would require the handler itself to
// hold the OS lock across the delete, which is agent-layer surgery
// outside this file's reach.
//
// Split into the App wrapper below plus externalSessionOwnerRefusalFromConfig
// so every branch is independently testable: an *appPkg.App with a non-nil
// store but empty DataDirectory cannot be built through app.New from this
// package (a config-Init'd store always resolves a directory via
// setDefaults, and a hand-built &config.Config{Options: &config.Options{}}
// panics inside app.New before returning), so the config-level guard is
// pinned at the FromConfig seam instead.
func externalSessionOwnerRefusal(a *appPkg.App, sessionID string) (refuse bool, message string) {
	if a == nil || a.Store() == nil {
		return true, "cannot verify external session ownership (no config store) — please retry"
	}
	return externalSessionOwnerRefusalFromConfig(a.Config(), sessionID)
}

// externalSessionOwnerRefusalFromConfig is the config-level half of
// externalSessionOwnerRefusal; see that function's doc for the fail-closed
// rules. Split out (task #622, third review) so tests can reach the
// nil-config / nil-Options / empty-DataDirectory guard directly — see the
// wrapper's doc for why it cannot be driven through a real App.
func externalSessionOwnerRefusalFromConfig(cfg *config.Config, sessionID string) (refuse bool, message string) {
	if cfg == nil || cfg.Options == nil || cfg.Options.DataDirectory == "" {
		return true, "cannot verify external session ownership (data directory unresolvable) — please retry"
	}
	st := session.InspectSessionLock(cfg.Options.DataDirectory, sessionID, externalOwnerLiveThreshold)
	if !st.Live {
		return false, ""
	}
	if st.PID != 0 && st.PID == os.Getpid() {
		// Provably ours: readable PID, this process, and (per the caller's
		// reservation + idle poll) our own finished turn's leftover lock.
		return false, ""
	}
	if st.PID == 0 {
		return true, "session may be owned by another process (live lock, holder PID unreadable) — wait for it to finish and retry"
	}
	return true, fmt.Sprintf("session is owned by another process (PID %d) — wait for it to finish and retry", st.PID)
}

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

	var smartOverride, fastOverride *agent.ModelOverride

	// Check payload first
	if p.SmartModel != nil {
		smartOverride = &agent.ModelOverride{Provider: p.SmartModel.Provider, Model: p.SmartModel.Model}
	}
	if p.FastModel != nil {
		fastOverride = &agent.ModelOverride{Provider: p.FastModel.Provider, Model: p.FastModel.Model}
	}

	// If no payload override, check DB
	if smartOverride == nil || fastOverride == nil {
		sess, err := a.Sessions.Get(ctx, p.SessionID)
		if err == nil {
			if smartOverride == nil && sess.SmartModelID != "" {
				slog.Info("ws: using models from DB", "sessionID", p.SessionID, "smart", sess.SmartModelID)
				smartOverride = &agent.ModelOverride{Provider: sess.SmartModelProvider, Model: sess.SmartModelID}
			}
			if fastOverride == nil && sess.FastModelID != "" {
				fastOverride = &agent.ModelOverride{Provider: sess.FastModelProvider, Model: sess.FastModelID}
			}
		}
	}

	if smartOverride != nil {
		slog.Info("ws: final models for run", "sessionID", p.SessionID, "smart", smartOverride.Model)
	}

	// Decouple the agent run from the WebSocket connection lifetime.
	// Without this, closing/refreshing the browser tab would cancel the agent.
	// Explicit cancellation is still available via Cancel(sessionID).
	agentCtx := context.WithoutCancel(ctx)

	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: p.SessionID, Busy: true})
	var err error
	if smartOverride != nil || fastOverride != nil {
		_, err = a.AgentCoordinator.RunWithOverrides(agentCtx, p.SessionID, p.Content, smartOverride, fastOverride, attachments...)
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
	var smartOverride, fastOverride *agent.ModelOverride
	if p.SmartModel != nil {
		smartOverride = &agent.ModelOverride{Provider: p.SmartModel.Provider, Model: p.SmartModel.Model}
	}
	if p.FastModel != nil {
		fastOverride = &agent.ModelOverride{Provider: p.FastModel.Provider, Model: p.FastModel.Model}
	}
	if smartOverride == nil || fastOverride == nil {
		if sess, err := a.Sessions.Get(ctx, p.SessionID); err == nil {
			if smartOverride == nil && sess.SmartModelID != "" {
				smartOverride = &agent.ModelOverride{Provider: sess.SmartModelProvider, Model: sess.SmartModelID}
			}
			if fastOverride == nil && sess.FastModelID != "" {
				fastOverride = &agent.ModelOverride{Provider: sess.FastModelProvider, Model: sess.FastModelID}
			}
		}
	}

	// Use bounded context for idle-interrupt: WithoutCancel + timeout to ensure
	// the operation can complete even if the WebSocket connection closes, but with
	// a reasonable upper bound to prevent indefinite hangs (e.g., blocked SQLite write).
	agentCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	if err := a.AgentCoordinator.InterruptAndSend(agentCtx, p.SessionID, p.Content, smartOverride, fastOverride, attachments...); err != nil {
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

	// 1. Cancel + clear queue if busy, then poll until idle (up to 10s). This
	// is a courtesy wait, NOT the safety mechanism: IsSessionBusy is a
	// snapshot, and a new Send/Rerun can legitimately start the instant after
	// this loop observes idle, before step 1a below claims exclusive
	// ownership. The wait exists purely so the common case (an old turn that
	// is already winding down) doesn't fail closed on step 1a just because
	// cancellation hasn't finished propagating yet.
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

	// Test-only seam (task #614 regression test): fires strictly AFTER the
	// idle-poll above observed the session idle and strictly BEFORE
	// ReserveExclusive below claims ownership — i.e. exactly the window a
	// pre-fix rerun would have proceeded through unprotected. A test can pause
	// here to start a concurrent new Send/Rerun and prove it observes the
	// session busy (or fails its own reservation) instead of racing this
	// handler's tail-delete loop. nil (a no-op) in every production path,
	// mirroring the mailbox package's testDrainSeam/testLoopRearmSeam idiom.
	if rerunPostIdlePollSeam != nil {
		rerunPostIdlePollSeam()
	}

	// 1a. Task #614 (F5 of the 2026-08-20 readonly release review): claim
	// EXCLUSIVE ownership of the session's mailbox before touching history.
	// This is the actual safety mechanism, not the idle-poll above: it is a
	// single atomic mbIdle->mbOwned transition (ReserveExclusive ->
	// mailbox.beginCompact), so there is no window between "observed idle"
	// and "became owner" for a concurrent Send/Rerun/InterruptAndSend to
	// slip through and start writing a new streaming assistant message while
	// this handler deletes the tail below. If the session is busy — even if
	// it raced busy again in the instant after the idle-poll above returned
	// — ReserveExclusive fails and this handler fails closed: nothing is
	// deleted, and the operator is told to retry rather than risking a
	// force-delete of a message a live new turn is writing.
	//
	// Ownership is held from here through the tail delete, the target
	// delete, and the handoff into RunWithReservedOwnership below — it is
	// never released and re-claimed in between, which is what closes the
	// F5 hole: releasing after delete and re-reserving for the replacement
	// Run would reopen exactly the same "another caller can become owner in
	// the gap" window this reservation exists to close.
	holdCtx, epoch, reserveCancel, reserved := a.AgentCoordinator.ReserveExclusive(ctx, sessionID)
	if !reserved {
		slog.Warn("ws: rerun: could not claim exclusive ownership (session became busy again)",
			"sessionID", sessionID, "messageID", p.MessageID)
		c.reply(msg.ID, EventError, nil, "session became busy again — please retry")
		return
	}
	// releaseOnBailout is true on every return path below UNTIL step 6 hands
	// ownership to RunWithReservedOwnership, which takes over final release
	// itself (mirroring Run()'s own single-release contract) — see the
	// defer's own check just before step 6.
	releaseOnBailout := true
	defer func() {
		if releaseOnBailout {
			a.AgentCoordinator.ReleaseExclusive(sessionID, epoch, reserveCancel)
		}
	}()

	// 1b. Task #622 (F-1): the reservation above proves no OTHER caller in
	// THIS process can be writing to the session, but says nothing about a
	// `crush run --session S` executing in a DIFFERENT process — its lock
	// is only consulted later, inside runOwned. Without this check, the
	// orphan-rescue branch in step 2 would happily force-delete a row that
	// a live external owner is still streaming into. Fail closed, same as
	// 6b2cd189 did for the in-process case: refuse the rerun with an
	// actionable message. externalSessionOwnerRefusal's own doc spells out
	// the three fail-closed rules (unknown-PID live lock, unresolvable
	// data directory, nil store) and the one window deliberately left
	// open: an owner acquiring the OS lock a moment AFTER the inspection.
	if refuse, why := externalSessionOwnerRefusal(a, sessionID); refuse {
		slog.Warn("ws: rerun: refusing: "+why,
			"sessionID", sessionID, "messageID", p.MessageID)
		c.reply(msg.ID, EventError, nil, why)
		return
	}

	// 2. Delete every message AFTER the target, keep the target.
	//
	// task #615: created_at is stored in whole SECONDS (see
	// internal/db/sql/messages.sql), so a message inserted just before the
	// target and one inserted just after can share the same created_at
	// value. Messages.List already returns the full session in a
	// deterministic (created_at ASC, rowid ASC) total order (same file,
	// ListMessagesBySession) — that rowid tiebreaker is exactly what a
	// timestamp-only comparison here would be missing. So instead of
	// re-deriving an order from timestamps (which cannot distinguish
	// same-second before/after), find the target's position in that
	// already-ordered list and delete only the slice strictly after it.
	allMsgs, listErr := a.Messages.List(holdCtx, sessionID)
	if listErr != nil {
		c.reply(msg.ID, EventError, nil, "failed to list messages")
		return
	}
	// Check if the hold was cancelled while listing messages
	if holdCtx.Err() != nil {
		c.reply(msg.ID, EventError, nil, "cancelled")
		return
	}
	targetIdx := -1
	for i, m := range allMsgs {
		if m.ID == targetMsg.ID {
			targetIdx = i
			break
		}
	}
	if targetIdx == -1 {
		// Fail closed: without a confirmed position in the ordered list we
		// cannot safely tell "before" from "after", so delete nothing.
		slog.Warn("ws: rerun: target message not found in session list",
			"sessionID", sessionID, "messageID", targetMsg.ID)
		c.reply(msg.ID, EventError, nil, "target message not found in session")
		return
	}

	// Test-only seam (task #614 regression test, reverse direction): fires
	// strictly AFTER ReserveExclusive above claimed ownership and strictly
	// BEFORE the tail-delete loop below runs. A test can pause here to prove
	// that a concurrent Send/Rerun attempted WHILE this handler holds the
	// reservation observes the session busy and cannot create a new
	// streaming message — the direction of the F5 race that actually matters
	// (a new turn starting mid-deletion), as opposed to
	// rerunPostIdlePollSeam's "someone else grabs the reservation before we
	// do" direction. nil (a no-op) in every production path.
	if rerunHoldingReservationSeam != nil {
		rerunHoldingReservationSeam()
	}

	for _, m := range allMsgs[targetIdx+1:] {
		if delErr := a.Messages.Delete(holdCtx, m.ID); delErr != nil {
			if errors.Is(delErr, message.ErrMessageStillStreaming) {
				// The message is still streaming, but THREE separate proofs
				// back the orphan claim: the session was cancelled and polled
				// to idle (step 1), this handler holds the exclusive
				// reservation (step 1a), and no OTHER process holds the
				// session's OS lock (step 1b). Only under all three can this
				// row truly never receive a terminal Finish — in-process idle
				// alone would not prove that across processes. Force-delete
				// it to avoid corrupting the transcript by including partial
				// text in LLM context forever.
				slog.Info("ws: rerun: orphaned streaming message, force-deleting",
					"id", m.ID, "err", delErr)
				if forceErr := a.Messages.ForceDelete(holdCtx, m.ID); forceErr != nil {
					slog.Warn("ws: rerun: failed to force-delete orphaned streaming message",
						"id", m.ID, "err", forceErr)
				}
			} else {
				slog.Warn("ws: rerun: failed to delete tail message", "id", m.ID, "err", delErr)
			}
		}
	}

	// Check if the hold was cancelled during tail deletion
	if holdCtx.Err() != nil {
		c.reply(msg.ID, EventError, nil, "cancelled")
		return
	}

	// 3. Delete the original user message — Run() will recreate it.
	if delErr := a.Messages.Delete(holdCtx, targetMsg.ID); delErr != nil {
		slog.Warn("ws: rerun: failed to delete original user message", "id", targetMsg.ID, "err", delErr)
	}

	// Check if the hold was cancelled while deleting the original message
	if holdCtx.Err() != nil {
		c.reply(msg.ID, EventError, nil, "cancelled")
		return
	}

	// 4. Re-arm Phase 4 autonomy.
	a.AgentCoordinator.ResetAutoResumeCounter(sessionID)

	// 5. Resolve model overrides (same priority as handleSendMessage).
	var smartOverride, fastOverride *agent.ModelOverride
	if sess, sessErr := a.Sessions.Get(holdCtx, sessionID); sessErr == nil {
		if sess.SmartModelID != "" {
			smartOverride = &agent.ModelOverride{Provider: sess.SmartModelProvider, Model: sess.SmartModelID}
		}
		if sess.FastModelID != "" {
			fastOverride = &agent.ModelOverride{Provider: sess.FastModelProvider, Model: sess.FastModelID}
		}
	}

	// 6. Run the agent with the same prompt, handing off the reservation
	// claimed in step 1a directly into the replacement turn — see
	// RunWithReservedOwnership's own doc for why this must CONTINUE the same
	// ownership era rather than release-then-reacquire. The onHandoff callback
	// transfers release responsibility: when it fires, the defer above is
	// disarmed and runOwned's defer takes over. Any panic before onHandoff fires
	// is still covered by the defer, which releases via ReleaseExclusive.

	// Test-only seam (task #614 F-2): fires right before Broadcast, i.e. before
	// RunWithReservedOwnership is called. Used to test that panics before the
	// onHandoff callback still release the reservation. nil (a no-op) in every
	// production path.
	if rerunPreHandoffSeam != nil {
		rerunPreHandoffSeam()
	}

	agentCtx := context.WithoutCancel(ctx)
	c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sessionID, Busy: true})
	if smartOverride != nil || fastOverride != nil {
		_, err = a.AgentCoordinator.RunWithReservedOwnership(agentCtx, sessionID, text, epoch, reserveCancel, func() { releaseOnBailout = false }, smartOverride, fastOverride)
	} else {
		_, err = a.AgentCoordinator.RunWithReservedOwnership(agentCtx, sessionID, text, epoch, reserveCancel, func() { releaseOnBailout = false }, nil, nil)
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
