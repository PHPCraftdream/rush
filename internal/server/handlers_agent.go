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

	"github.com/PHPCraftdream/rush/internal/agent"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
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
// defaults to "<workingDir>/.rush" but honors an explicit --data-dir or
// configured data_directory) — callers must not append ".rush" themselves.
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
	// atomic counter is only unique WITHIN one process, but multiple rush
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
	// Web-originated turn: tag the entry channel so the user message the
	// agent creates carries OriginWeb (decoupled from the WS lifetime —
	// WithoutCancel still applies).
	agentCtx := agent.WithCallOrigin(context.WithoutCancel(ctx), message.OriginWeb)

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
	agentCtx, cancel := context.WithTimeout(agent.WithCallOrigin(context.WithoutCancel(ctx), message.OriginWeb), 30*time.Second)
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

	agentCtx := agent.WithCallOrigin(context.WithoutCancel(ctx), message.OriginWeb)
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
// attachments. It defensively falls back to "<workingDir>/.rush" (the
// pre-fix, cwd-derived default) on the rare nil-config edge case, so a
// missing config doesn't turn a best-effort attachment save into a hard
// failure.
func attachmentsDataDir(a *appPkg.App) string {
	return cmp.Or(externalOwnershipDataDir(a), filepath.Join(a.Store().WorkingDir(), ".rush"))
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
	var sess session.Session
	var createErr error
	if oc, ok := a.Sessions.(session.OriginCreator); ok {
		sess, createErr = oc.CreateWithOrigin(ctx, "Project Initialization", message.OriginWeb)
	} else {
		// Test fakes without the OriginCreator seam keep the legacy path.
		sess, createErr = a.Sessions.Create(ctx, "Project Initialization")
	}
	err = createErr
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
	agentCtx := agent.WithCallOrigin(context.WithoutCancel(ctx), message.OriginWeb)
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
