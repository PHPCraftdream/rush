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

// rerunTailDeleteSeam is a test-only hook (task #630): fires at the top
// of every tail-deletion loop iteration in handleRerunMessage, with the
// iteration index, BEFORE that iteration's Delete call. A test can cancel
// the reservation hold at i==1 to land a Cancel precisely BETWEEN two tail
// deletions and assert the tail is never left half-deleted. nil (a no-op)
// in every production path.
var rerunTailDeleteSeam func(i int)

// rerunPreTargetDeleteSeam is a test-only hook (task #630 follow-up): fires
// immediately BEFORE step 3 deletes the target user message, i.e. strictly
// after the last honoured cancellation check and strictly after the tail
// loop. A test can cancel the hold here to prove the handler is already
// committed: it must proceed to the replacement turn, never return with the
// user's own message deleted and no rerun. nil in every production path.
var rerunPreTargetDeleteSeam func()

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

// holdExternalSilenceProof acquires the kernel-attested proof that no
// external process owns sessionID: a non-blocking SHARED OS lock on the
// session's lock file (session.TryHoldSessionLockShared), which the caller
// must HOLD across its history-destructive work and Release afterwards.
// It replaces ce3b418e's byte-heuristic (InspectSessionLock mtime/PID
// inference) on the destructive path, per the #631 redesign: any encoding
// of "held" vs "released" in disk bytes has an irreducible race — a
// just-acquired holder whose PID hasn't landed in the file yet, a released
// leftover whose truncate refreshed the mtime — while a shared range lock
// conflicts with every exclusive holder on every platform and retracts
// atomically with both process death and lock acquisition. Winning the
// probe therefore proves no exclusive holder exists at this instant, AND —
// because shared and exclusive range locks conflict — that none can appear
// while it is held, which closes the old "owner arrives a moment after the
// inspection" residual window too (the probe's O_CREATE pins the inode so
// an arriving acquirer opens the same file and fails against us).
//
// Semantics, for the four states this file's history has enumerated:
// released leftover (empty file, no sidecar) grants the probe — nobody
// owns it; held-with-unreadable-identity (Windows mandatory lock, no
// sidecar) denies the probe — a live owner exists regardless of what the
// bytes say; held-with-known-PID denies, whether foreign OR OUR OWN —
// strictly safer than the old heuristic's own-PID allow, since a genuinely
// held in-process lock means a turn is running; mid-acquire (bytes still
// empty) denies — the kernel already holds the exclusive lock even though
// the disk hasn't caught up.
//
// Fail-closed rules carried over from #622:
//
//  1. Contention refuses with an actionable message. The holder PID in the
//     message is best-effort (sidecar, then primary; on Windows the
//     primary is unreadable while held) and may be 0 or our own — the
//     refusal itself does not depend on it.
//  2. A data directory that cannot be resolved (nil store/config/options
//     or empty DataDirectory) refuses outright. attachmentsDataDir paper
//     over this edge with a workingDir fallback because a wrong guess only
//     misplaces files; here a wrong guess probes the wrong locks/
//     directory, i.e. fails open. "Could not look" must not read as
//     "looked and found nothing".
//  3. Any probe error that is not contention (permission, IO, filesystems
//     where range locks don't work — some NFS/SMB) refuses: the exclusive
//     acquire path fails on such filesystems too, so refusing here is
//     consistent, not a regression.
//
// The nil-store case is included in rule 2 rather than special-cased: if
// it is unreachable in production the fail-closed branch costs nothing,
// and if it is reachable it is exactly rule 2.
//
// Split into the App wrapper below plus holdExternalSilenceProofFromConfig
// so every branch is independently testable: an *appPkg.App with a non-nil
// store but empty DataDirectory cannot be built through app.New from this
// package (a config-Init'd store always resolves a directory via
// setDefaults, and a hand-built &config.Config{Options: &config.Options{}}
// panics inside app.New before returning), so the config-level guard is
// pinned at the FromConfig seam instead.
func holdExternalSilenceProof(a *appPkg.App, sessionID string) (probe *session.SharedLockProbe, refuse bool, message string) {
	if a == nil || a.Store() == nil {
		return nil, true, "cannot verify external session ownership (no config store) — please retry"
	}
	return holdExternalSilenceProofFromConfig(a.Config(), sessionID)
}

// holdExternalSilenceProofFromConfig is the config-level half of
// holdExternalSilenceProof; see that function's doc for the fail-closed
// rules. Split out (task #622, third review) so tests can reach the
// nil-config / nil-Options / empty-DataDirectory guard directly — see the
// wrapper's doc for why it cannot be driven through a real App.
func holdExternalSilenceProofFromConfig(cfg *config.Config, sessionID string) (*session.SharedLockProbe, bool, string) {
	if cfg == nil || cfg.Options == nil || cfg.Options.DataDirectory == "" {
		return nil, true, "cannot verify external session ownership (data directory unresolvable) — please retry"
	}
	probe, err := session.TryHoldSessionLockShared(cfg.Options.DataDirectory, sessionID)
	if err == nil {
		return probe, false, ""
	}
	var busyErr *session.SessionLockBusyError
	if errors.As(err, &busyErr) {
		if busyErr.HolderPID != 0 {
			return nil, true, fmt.Sprintf("session is owned by another process (PID %d) — wait for it to finish and retry", busyErr.HolderPID)
		}
		return nil, true, "session is owned by another process (holder identity unreadable) — wait for it to finish and retry"
	}
	return nil, true, "cannot verify external session ownership (lock probe failed: " + err.Error() + ") — please retry"
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

	// 1b. Task #622 (F-1), redesigned in #631: the reservation above proves
	// no OTHER caller in THIS process can be writing to the session, but
	// says nothing about a `crush run --session S` executing in a DIFFERENT
	// process. The old byte-heuristic inspection is replaced by a
	// kernel-attested SHARED lock probe on the session's lock file,
	// HELD from here through the tail delete and the target delete below:
	// while the shared lock is held, no process — including this one — can
	// acquire the exclusive lock, so no external writer can start mid-
	// delete. It is released just before the step-6 handoff, because the
	// replacement turn's own Run() takes the exclusive lock at its normal
	// acquire point and a shared lock still held here would conflict with
	// our own acquire. holdExternalSilenceProof's doc spells out the
	// fail-closed rules (contention, unresolvable data directory, probe
	// error).
	probe, refuse, why := holdExternalSilenceProof(a, sessionID)
	if refuse {
		slog.Warn("ws: rerun: refusing: "+why,
			"sessionID", sessionID, "messageID", p.MessageID)
		c.reply(msg.ID, EventError, nil, why)
		return
	}
	// probeHeld guards the bailout paths between here and the step-6
	// handoff. probe is never nil here: the refuse branch above (the only
	// path holdExternalSilenceProof can return a nil probe on) already
	// returned before this defer is registered, and TryHoldSessionLockShared
	// opens with os.O_CREATE, so even an absent lock file yields a held,
	// non-nil probe on every path that reaches this point.
	// probe.Release's own nil-safety is defensive belt-and-suspenders, not
	// load-bearing for any path reachable from here today.
	probeHeld := true
	defer func() {
		if probeHeld {
			probe.Release()
		}
	}()

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

	// Final phase-boundary check before mutating history: once the loop
	// below is entered it runs to completion (see deleteCtx), so a Cancel
	// must be honoured HERE — tail untouched — rather than mid-loop, where
	// honouring it would leave the tail half-deleted with no rerun in
	// exchange (task #630).
	if holdCtx.Err() != nil {
		c.reply(msg.ID, EventError, nil, "cancelled")
		return
	}

	// deleteCtx is the context the tail-deletion loop and step 3's target
	// delete run under. It is holdCtx with cancellation stripped
	// (context.WithoutCancel): a Cancel landing between two loop iterations
	// must not kill the remaining Delete/ForceDelete calls, or the session's
	// history is left partially truncated and the post-loop check replies
	// "cancelled" with nothing to show for the lost messages (task #630).
	// The invariant: the tail is either untouched (cancel honoured at a
	// phase boundary above) or fully deleted — never half. WithoutCancel
	// rather than context.Background() to keep holdCtx's values, matching
	// the agentCtx idiom already used further down in this handler.
	deleteCtx := context.WithoutCancel(holdCtx)

	for i, m := range allMsgs[targetIdx+1:] {
		// Test-only seam (task #630): see rerunTailDeleteSeam's declaration.
		if rerunTailDeleteSeam != nil {
			rerunTailDeleteSeam(i)
		}
		if delErr := a.Messages.Delete(deleteCtx, m.ID); delErr != nil {
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
				if forceErr := a.Messages.ForceDelete(deleteCtx, m.ID); forceErr != nil {
					slog.Warn("ws: rerun: failed to force-delete orphaned streaming message",
						"id", m.ID, "err", forceErr)
				}
			} else {
				slog.Warn("ws: rerun: failed to delete tail message", "id", m.ID, "err", delErr)
			}
		}
	}

	// Check if the hold was cancelled during tail deletion. This is the
	// LAST cancellation point the handler honours (task #630): honouring
	// here leaves the state "tail fully deleted, target intact, no rerun"
	// — the operator's own prompt survives and a retry re-enters cleanly.
	// Every step after this mutates the target itself; once step 3 has
	// deleted it, only the replacement turn's Run() can recreate it, so
	// from here on a Cancel is NOT honoured in this handler. It is already
	// delivered to the mailbox (the holdCancel WAS the cancel target),
	// where the agent layer rebinds and applies it to the replacement turn
	// instead — the correct place for a mid-rerun Cancel to land.
	if holdCtx.Err() != nil {
		c.reply(msg.ID, EventError, nil, "cancelled")
		return
	}

	// Test-only seam (task #630 follow-up): see rerunPreTargetDeleteSeam's
	// declaration.
	if rerunPreTargetDeleteSeam != nil {
		rerunPreTargetDeleteSeam()
	}

	// 3. Delete the original user message — Run() will recreate it. Runs
	// under deleteCtx (cancellation stripped) and is the COMMIT POINT: if
	// this Delete succeeds and the handler returned early, the user's own
	// words would be gone with nothing recreating them, so nothing below
	// may return without first handing off into RunWithReservedOwnership.
	// A Cancel arriving during or after this delete is left to the agent
	// layer: RunWithReservedOwnership is invoked with agentCtx (derived
	// from the request ctx, not holdCtx) and continues the same ownership
	// era regardless of the hold's cancellation state.
	if delErr := a.Messages.Delete(deleteCtx, targetMsg.ID); delErr != nil {
		slog.Warn("ws: rerun: failed to delete original user message", "id", targetMsg.ID, "err", delErr)
	}

	// Capture the SET of message IDs the recreate watermark compares
	// against (task #655, fourteenth-review P2-1/P3-1; seeded from the
	// pre-delete listing by #658, fifteenth-review P3-1). This is not an
	// exclusivity claim: the reservation and probe above exclude other
	// agent RUNS, but handleDeleteMessage, handleDeleteMessages and
	// handleUpdateMessageContent (handlers_messages.go) — all three of
	// which can only delete or edit an existing row — and
	// handleInjectMessage (via coordinator.InjectMessage, the only one
	// of the four that can ADD a row: agent Run cannot land one here
	// because it reserves the session before writing any row) mutate
	// rows with no ownership check and may interleave here. The set does not need
	// them excluded: a concurrent writer only adds a row whose ID was
	// never in the set or removes one that was, and ID membership —
	// unlike the index or count #651 used — is invariant under deletions
	// anywhere in the list, which is exactly what in-turn compaction
	// (deleting summarised rows below the replacement turn's prompt)
	// invalidates positionally.
	//
	// The set is SEEDED from allMsgs — the pre-delete listing captured at
	// step 2, still in scope here — and the post-delete List's IDs are
	// unioned in when that call succeeds. Every row in allMsgs predates
	// the replacement run, so the seed alone is a valid (superset)
	// baseline, and seeding UNCONDITIONALLY means the set is never nil:
	// the helper's scan always runs instead of it creating blind. On the
	// normal path the union changes nothing — for any row still present
	// when the helper scans, membership in the union equals membership in
	// the post-delete set alone, because a row that predates the run and
	// still exists was necessarily in the post-delete listing too. The
	// seed's extra IDs are rows the deletes removed (a nonexistent ID
	// suppresses nothing) or, when a tail delete failed, pre-existing
	// rows that must not suppress the recreate anyway — exactly what set
	// membership already gives them. The target's own ID is in the seed
	// unconditionally, and the helper still admits it via its explicit
	// targetID disjunct.
	//
	// When the post-delete List fails (e.g. a transient DB error that
	// clears before the helper runs seconds later), the set falls back to
	// the pre-delete seed alone, so the scan still recognises the
	// replacement turn's own createUserMessage row (its ID is not in the
	// seed) instead of appending a duplicate next to it once the turn
	// later errors. The residual gap is a row created by a concurrent
	// writer BETWEEN the two listings: on this failed-List path it is in
	// neither set, so a same-text foreign row could suppress the
	// recreate — the same concurrent-writer tolerance the helper's doc
	// documents. That window is not two adjacent statements: it spans
	// the whole tail-delete loop plus step 3's target delete — one
	// Messages.Delete (itself a get + delete-if-terminal + publish
	// round trip) per tail row — so it is milliseconds for a short tail
	// and hundreds of milliseconds for a long one.
	baselineIDs := make(map[string]struct{}, len(allMsgs))
	for _, m := range allMsgs {
		baselineIDs[m.ID] = struct{}{}
	}
	if msgs, listErr := a.Messages.List(deleteCtx, sessionID); listErr == nil {
		for _, m := range msgs {
			baselineIDs[m.ID] = struct{}{}
		}
	} else {
		slog.Warn("ws: rerun: failed to list messages after target delete; baseline ID set falls back to the pre-delete listing",
			"sessionID", sessionID, "err", listErr)
	}

	// Task #645 (twelfth-review N-2): recreate the user prompt if it was
	// lost, on every exit path past the commit point above that did not
	// reach a returned run — including a PANIC anywhere between the delete
	// and a normal return: before the handoff (e.g. at rerunPreHandoffSeam
	// or in hub.Broadcast) and, since task #655 (fourteenth-review M-2), in
	// the window where onHandoff has fired but the replacement turn's
	// createUserMessage has not run yet (onHandoff fires before runOwned).
	// Both unwind with runReturned still false, keeping this defer armed.
	// Registered strictly AFTER the delete so it never fires for the early
	// returns above (before step 3 there is nothing to recreate). It runs
	// before the releaseOnBailout/probeHeld defers (LIFO), so on panic
	// unwind it still runs while the reservation is held. On ordinary
	// returns the agent layer has already released, so correctness does NOT
	// depend on exclusivity — it depends on the gating here plus the
	// baseline-ID+text scan in recreateRerunPromptIfLost, which no
	// unrelated concurrent writer can spoof.
	//
	// The gate, restated from #651: a successful run must never be
	// second-guessed — its transcript, including any in-turn compaction, is
	// the completed turn's own business — so runReturned flips to true the
	// moment RunWithReservedOwnership RETURNS, disarming this defer on the
	// success path exactly as #651's releaseOnBailout gate did, and an
	// error return disarms it via recreateHandled after the explicit call
	// in the err != nil branch below. releaseOnBailout alone could not
	// express the M-2 window: onHandoff has already cleared it there, so
	// #651's gate let the prompt be lost.
	recreateHandled := false
	runReturned := false
	defer func() {
		if (releaseOnBailout || !runReturned) && !recreateHandled {
			recreateHandled = true
			recreateRerunPromptIfLost(deleteCtx, a, sessionID, baselineIDs, targetMsg.ID, text)
		}
	}()

	// 4. Re-arm Phase 4 autonomy.
	a.AgentCoordinator.ResetAutoResumeCounter(sessionID)

	// 5. Resolve model overrides (same priority as handleSendMessage).
	var smartOverride, fastOverride *agent.ModelOverride
	// Sessions.Get is read-only, but run it under deleteCtx too: holdCtx
	// may already be cancelled past the commit point, and failing the Get
	// would silently drop the session's model overrides for no reason.
	if sess, sessErr := a.Sessions.Get(deleteCtx, sessionID); sessErr == nil {
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

	// Release the shared-ownership probe BEFORE the step-6 handoff: the
	// replacement turn's Run() takes the EXCLUSIVE session lock at its
	// normal acquire point (agent_run.go, runOwned), and a shared lock
	// still held by this process would conflict with our own acquire and
	// bounce the rerun we just set up. Everything destructive is done —
	// the delete completed while provably owner-free — so releasing here
	// reopens only the benign bounce window between this release and the
	// acquire inside RunWithReservedOwnership (see holdExternalSilenceProof's
	// doc: no data can be destroyed by an owner arriving after the commit
	// point). Released before the rerunPreHandoffSeam below so the seam —
	// which represents the handoff instant — observes no probe held.
	probe.Release()
	probeHeld = false

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

	// The replacement run has RETURNED (success or error): from here the
	// recreate defer is disarmed — on success the completed turn owns the
	// transcript, on error the explicit call below handles the prompt. Every
	// panic path (pre-handoff, the handoff→createUserMessage window, and
	// mid-run) still unwinds with runReturned false and stays covered.
	runReturned = true

	// P2-2 fix: broadcast the actual busy state derived from mailbox ownership,
	// not from this request handler's lifetime.
	if !a.AgentCoordinator.IsSessionBusy(sessionID) {
		c.hub.Broadcast(EventAgentBusy, AgentBusyPayload{SessionID: sessionID, Busy: false})
	}

	if err != nil {
		slog.Error("ws: rerun agent error", "err", err)
		// Task #638/#644: recreate the user prompt if it was lost — see the
		// defer registered after step 3's delete for the full rationale.
		// Run it explicitly here (BEFORE the error reply) so a watching
		// client still sees the recreated message's CreatedEvent broadcast
		// ahead of the error reply, matching the pre-#645 behaviour; the
		// defer no-ops afterwards via recreateHandled.
		// The flag is set AFTER the call (fourteenth-review M-5): if the
		// call itself panics (List/Create on a closed DB), recreateHandled
		// stays false and the defer below runs on the unwind — but it can
		// retry only on the error returns where onHandoff never fired
		// (RunWithReservedOwnership's pre-handoff failures), where
		// releaseOnBailout is still true and the defer's gate is open. On
		// THIS dominant error path onHandoff has already fired and
		// runReturned is true, so the gate is closed regardless: a panic
		// in the call loses the prompt either way (28f37afc behaved
		// identically here), and the ordering matters only for that
		// narrow pre-handoff path.
		recreateRerunPromptIfLost(deleteCtx, a, sessionID, baselineIDs, targetMsg.ID, text)
		recreateHandled = true
		c.reply(msg.ID, EventError, nil, err.Error())
		return
	}
	c.reply(msg.ID, EventResponse, map[string]string{"status": "ok"}, "")
}

// recreateRerunPromptIfLost restores a rerun target deleted at step 3 of
// handleRerunMessage when the replacement turn never recreated it (task
// #638/#644, extended to panic paths by #645, redesigned around message-ID
// membership by #655 after the fourteenth review showed both the count- and
// position-based watermarks break when rows are deleted below the prompt,
// and made never-nil by #658 seeding the capture set from the pre-delete
// listing).
// The check is ID-MEMBERSHIP+TEXTUAL: a row proves the prompt is present
// iff it is a User message whose Content().Text equals the captured prompt
// text AND either its ID was NOT in the baseline set captured around the
// target delete (so it was written after the baseline was captured —
// normally the replacement turn's createUserMessage row or an earlier
// explicit call's, though not ONLY those: a concurrent writer with no
// ownership check, e.g. handleInjectMessage, can land a same-text row in
// that window too) or its ID IS the original target's (step 3's delete
// failed and the operator's own row survived untouched — already present,
// nothing to restore). Membership
// never depends on order or count, so deletions elsewhere in the list —
// in-turn compaction deleting summarised rows below the prompt, a
// concurrent handleDeleteMessage — cannot shift or spoof the watermark the
// way #651's index window was shifted. Unrelated concurrent writers are
// still ignored (their text does not match), and an EARLIER identical
// prompt that survived the tail delete (#644's "continue" shape) still does
// not suppress the recreate: its ID is in the baseline set and it is not
// the target. Idempotent: once a qualifying row exists, this no-ops.
//
// baselineIDs is never nil from the capture site (task #658): it is
// seeded from the pre-delete listing and unioned with the post-delete
// one, so even a failed post-delete List leaves a valid superset
// baseline and the scan always runs. There is deliberately no "watermark
// unknown, create unconditionally" branch: it minted a spurious
// duplicate whenever a transient List failure cleared before a
// replacement run that had written its own prompt errored
// (fifteenth-review P3-1), and the seed removes the need to choose
// between creating blind and suppressing blind.
func recreateRerunPromptIfLost(ctx context.Context, a *appPkg.App, sessionID string, baselineIDs map[string]struct{}, targetID string, text string) {
	allMsgs, listErr := a.Messages.List(ctx, sessionID)
	if listErr != nil {
		slog.Error("Failed to list messages while checking if prompt needs recreation",
			"sessionID", sessionID, "err", listErr)
		return
	}

	// A qualifying row — User, matching text, and either new since the
	// baseline or the never-deleted target itself — means the prompt is
	// already present; return without creating.
	for _, m := range allMsgs {
		_, inBaseline := baselineIDs[m.ID]
		if m.Role == message.User && m.Content().Text == text && (!inBaseline || m.ID == targetID) {
			return
		}
	}

	_, createErr := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: text}},
	})
	if createErr != nil {
		slog.Error("Failed to recreate lost user prompt",
			"sessionID", sessionID, "err", createErr)
	}
}
