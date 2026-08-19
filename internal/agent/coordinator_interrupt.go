// Cancellation, cross-process interrupt-inject polling, detached durable
// enqueueing, and message injection. Extracted from coordinator.go — pure
// code move, bodies unchanged.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/google/uuid"
)

// interruptInjectTick is how often the interrupt-inject ticker polls
// pending_injects for interrupt=true rows during an active turn. 3s is a
// deliberate middle ground: fast enough that `crush sessions inject
// --interrupt` feels near-immediate to an operator (worst case one tick of
// latency), slow enough that the extra SELECT is negligible even across a
// long multi-step turn. The ticker only lives for the duration of a turn (see
// startInterruptTicker), so there is no idle-process polling.
const interruptInjectTick = 3 * time.Second

// interruptTickOperationTimeout is the per-tick deadline for the
// handleInterruptTick operation. If a tick's DB operations or downstream
// calls block longer than this, the tick is abandoned with a timeout error
// and the goroutine returns to the select loop to observe ctx.Done().
// P1-3 fix: prevents a single blocking tick from permanently hanging
// coordinator shutdown when the parent ctx is cancelled.
// 10s is chosen as ~3x the tick interval — long enough that normal operation
// never times out (a healthy tick completes in <<1s), but short enough that
// a genuinely stuck tick doesn't block shutdown for an unreasonable duration.
const interruptTickOperationTimeout = 10 * time.Second

func (c *coordinator) Cancel(sessionID string) {
	c.currentAgent.Cancel(sessionID)
}

func (c *coordinator) CancelAll() (stillBusy bool) {
	return c.currentAgent.CancelAll()
}

func (c *coordinator) ClearQueue(sessionID string) {
	c.currentAgent.ClearQueue(sessionID)
}

// startInterruptTicker launches a goroutine that polls pending_injects for an
// interrupt=true row for sessionID every interruptInjectTick, for as long as
// ctx is live (i.e. the duration of the owning turn). On each interrupt row
// it consumes it, requeues the already-persisted message via
// ConsumeInterruptInjectAndEnqueue, and cancels the running generation.
//
// CORRECTED (task #421/P0-1; the original text here claimed "a replacement
// turn runs under the same coordinator-level Run", which stopped being true
// once handleInterruptTick started marking calls FromDurableQueue=true —
// see mailbox.go's guard on mb.replacement): for a durable-queue-originated
// interrupt, cancelling the current generation does NOT hand this Run() call
// a replacement turn to keep running — sessionAgent.Run's turn loop simply
// ends (hasNext=false), and this ticker's own ctx (the owning Run call's)
// is cancelled right along with it, so the ticker exits too. The durable row
// is the only remaining owner of the interrupted work; it is executed
// SEPARATELY, in the same OS process but outside this Run call, by
// RunNonInteractive's DrainSessionNow call (internal/app/app.go) once this
// Run() returns — not by this ticker continuing to poll for it. The one
// case this ticker DOES keep ticking across is a NON-durable interrupt
// (InterruptAndSend, still sets mb.replacement): that replacement genuinely
// does run under this same Run call, and remains interruptible by a second
// cross-process interrupt via this same ticker, exactly as originally
// documented. The goroutine also exits when ctx is cancelled (turn finished/
// aborted), so it never outlives the turn either way.
//
// Returns a channel that is closed when the ticker goroutine exits. Callers
// should defer a receive from this channel to ensure the goroutine has fully
// joined before returning, avoiding in-flight handleInterruptTick execution
// after context cancellation and DB cleanup.
func (c *coordinator) startInterruptTicker(ctx context.Context, sessionID string) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interruptInjectTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// P1-3 fix: give each tick a bounded operation deadline so that
				// even if handleInterruptTick blocks (e.g., on a DB call or
				// downstream dependency that ignores parent ctx cancellation),
				// the tick returns within interruptTickOperationTimeout and we
				// can observe ctx.Done() on the next loop iteration.
				tickCtx, tickCancel := context.WithTimeout(ctx, interruptTickOperationTimeout)
				_, err := c.handleInterruptTick(tickCtx, sessionID)
				tickCancel()

				if err != nil {
					if errors.Is(err, context.DeadlineExceeded) {
						// This is a signal that some operation inside handleInterruptTick
						// blocked without respecting ctx cancellation. Log at warning level
						// so it's visible in production and warrants investigation, but don't
						// stop the ticker — subsequent ticks may succeed.
						slog.Warn("coordinator: interrupt-inject tick timed out",
							"session_id", sessionID, "timeout", interruptTickOperationTimeout)
					} else {
						slog.Warn("coordinator: interrupt-inject tick failed",
							"session_id", sessionID, "err", err)
					}
					continue
				}
				// Continue ticking. Whether a replacement turn "remains
				// interruptible by this same ticker" depends on which path
				// fired above — see startInterruptTicker's own doc for the
				// distinction (task #421/P0-1 correction): a durable-queue
				// interrupt has no replacement turn under THIS Run call at
				// all (the row is executed separately, after Run returns),
				// so this loop iteration is really just clearing the way for
				// the ctx-cancellation exit that follows shortly. A
				// non-durable interrupt's replacement genuinely does run
				// here and stays covered by this same ticker. Either way,
				// subsequent interrupts are handled correctly (the durable
				// queue serves FIFO ordering regardless of which path
				// consumes it).
			}
		}
	}()
	return done
}

// handleInterruptTick performs one poll of the interrupt-inject queue. It
// returns fired=true when it consumed an interrupt row and issued a
// cancel+requeue (the caller then stops ticking). Extracted from the ticker
// goroutine so it can be unit-tested directly with a real session.Service and
// message.Service, without a live provider. It is a no-op returning
// (false, nil) when no interrupt row is pending.
//
// P0-2 fix (atomic): uses ConsumeInterruptInjectAndEnqueue to delete and
// enqueue in a single transaction, eliminating the data loss window where
// a separate delete-then-enqueue sequence could lose the row if enqueue failed.
// This approach requires building the call data BEFORE the atomic transaction.
// Every fallible step (messages.Get, buildCall, marshal) runs BEFORE the
// atomic consume — PeekInterruptInject does not delete, so a failure there
// simply leaves the row in place for the next tick to retry naturally; no
// explicit recreation is needed. Once ConsumeInterruptInjectAndEnqueue
// commits, the call is durably enqueued and nothing after that point
// (Notify, InterruptAndReplace) can lose it.
func (c *coordinator) handleInterruptTick(ctx context.Context, sessionID string) (bool, error) {
	// First, peek at the row to get the message reference (SELECT only, no delete)
	pi, err := c.sessions.PeekInterruptInject(ctx, sessionID)
	if err != nil {
		return false, err
	}
	if pi == nil {
		return false, nil
	}

	// Load the message to build call data
	injMsg, getErr := c.messages.Get(ctx, pi.MessageID)
	if getErr != nil {
		return false, fmt.Errorf("interrupt inject references missing message %q: %w", pi.MessageID, getErr)
	}

	// Resolve the session's model configuration from the DB or config
	// defaults so this cross-process interrupt tick respects a persisted
	// per-session model override instead of falling back to the shared/
	// global model (same class of bug as InterruptAndSend,
	// closed for that call site separately).
	pinned, resolveErr := c.resolveSessionModels(ctx, sessionID)
	if resolveErr != nil {
		return false, fmt.Errorf("failed to resolve session models for interrupt tick: %w", resolveErr)
	}

	// Build the call
	call, buildErr := c.buildCall(ctx, sessionID, injMsg.FullText(), pinned, nil)
	if buildErr != nil {
		return false, buildErr
	}

	// Reference the existing row; the agent must not re-create it.
	call.ExistingMessageID = pi.MessageID
	call.InjectID = pi.ID

	// Mark as originating from the durable queue so InterruptAndReplace skips
	// mb.replacement to avoid double-execution (P0-1 fix). The pump will execute
	// the durable row directly; we only cancel the in-flight generation here.
	call.FromDurableQueue = true

	// Generate idempotency key
	var idempotencyKey string
	if call.LogicalCallID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.LogicalCallID)
	} else {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.InjectID)
	}

	// Convert to SessionAgentCallData for serialization
	callData := ToSessionAgentCallData(call)
	callDataJSON, marshalErr := json.Marshal(callData)
	if marshalErr != nil {
		return false, fmt.Errorf("failed to serialize call data for interrupt inject: %w", marshalErr)
	}

	// Now atomically consume (delete) and enqueue in one transaction, matching
	// the exact row peeked above so a concurrent deletion of that row (rather
	// than a stale re-select of "the oldest row") can never cause us to
	// consume a different row than the one callData was built from.
	enqueuedPi, enqueueErr := c.sessions.ConsumeInterruptInjectAndEnqueue(ctx, sessionID, pi.ID, idempotencyKey, callDataJSON)
	if enqueueErr != nil {
		// Transaction rolled back, so row still exists for retry
		return false, fmt.Errorf("failed to enqueue interrupt inject: %w", enqueueErr)
	}
	if enqueuedPi == nil {
		// Row vanished between peek and enqueue — handled gracefully
		return false, nil
	}

	// Notify the message so web UI renders it live
	c.messages.Notify(injMsg)

	// InterruptAndReplace atomically records call and cancels only the
	// in-flight generation (design §4). Since we've already enqueued durably,
	// we just need to cancel the in-flight generation if there is one.
	if !c.currentAgent.InterruptAndReplace(sessionID, call) {
		// No owner — session is idle, the durable enqueue already handles it
		slog.Debug("coordinator: interrupt tick enqueued durable call for idle session",
			"session_id", sessionID, "idempotency_key", idempotencyKey)
	}
	return true, nil
}

// InterruptAndSend queues a user message and cancels the running turn.
// agent.Run()'s cancel-handling branch drains the queue and the queued
// message becomes the next Run() — with all assistant content produced so
// far preserved in the DB (the cancel path writes a FinishReasonCanceled
// to the in-flight assistant message before unwinding).
func (c *coordinator) InterruptAndSend(ctx context.Context, sessionID, prompt string, smart, fast *ModelOverride, attachments ...message.Attachment) error {
	if err := c.readyWg.Wait(); err != nil {
		return err
	}
	var pinned *resolvedOverrides
	if smart != nil || fast != nil {
		resolved, applyErr := c.applyModelOverrides(ctx, smart, fast)
		if applyErr != nil {
			return applyErr
		}
		pinned = resolved
	} else {
		// No explicit overrides: resolve from session DB or config defaults.
		// This ensures that an interrupt respects the session's persisted model
		// override (if any) rather than falling back to the shared/global model.
		resolved, resolveErr := c.resolveSessionModels(ctx, sessionID)
		if resolveErr != nil {
			return fmt.Errorf("failed to resolve session models for interrupt: %w", resolveErr)
		}
		pinned = resolved
	}
	call, err := c.buildCall(ctx, sessionID, prompt, pinned, attachments)
	if err != nil {
		return err
	}
	// InterruptAndReplace atomically records call as the replacement the
	// current owner runs next and cancels only the in-flight generation
	// (design §4) — replacing the QueueMessage+Cancel two-step that P0-2
	// made self-defeating (Cancel deterministically wiped what QueueMessage
	// just queued). When the session is idle there is nothing to interrupt,
	// and nobody is running who would ever drain a queued call — so we must
	// start the run ourselves (P0-B).
	if !c.currentAgent.InterruptAndReplace(sessionID, call) {
		if err := c.startDetachedRun(ctx, call); err != nil {
			return fmt.Errorf("failed to enqueue interrupt for idle session %s: %w", sessionID, err)
		}
	}
	return nil
}

// startDetachedRun durably enqueues call for the idle-session paths of
// InterruptAndSend (P0-B). Despite the name (kept
// for git-blame continuity with the pre-#340 version), it no longer runs
// call itself, in a goroutine or otherwise — see the task #340 paragraph
// below for what changed and why.
//
// Those paths used to call QueueMessage(call) when InterruptAndReplace
// reported no owner. That is a runnerless queue: with the session idle
// there is no turn loop left to drain it, so the call sat there until
// some unrelated future Run() happened to come along — while the caller
// (and, through it, the web client) had already been told the message was
// "queued". For a user pressing interrupt on a session that had just
// finished, or landing in the race right after a release, the message
// simply never ran. Durably enqueuing here is what the mailbox's own
// contract says the caller must do when it is handed "no owner" (see
// mailbox.interruptAndReplace's doc).
//
// Task #340, ROUND 3 migration: durably enqueues the call to the
// session_run_queue table (session.EnqueueRunQueueEntry) synchronously, in
// the CALLER's own goroutine and ctx — no longer spawns its own goroutine
// and no longer wraps ctx in context.WithoutCancel. The independent
// RunQueuePump is what actually executes the call later; this function's
// job ends once the durable-enqueue write has committed (or, on enqueue
// failure, once the pending_injects row has been recreated below). This
// eliminates data loss risks from the previous bounded-retry-then-log
// approach and ensures every accepted call gets a guaranteed runner (or an
// explicit terminal failure recorded in the queue), even across process
// restarts.
//
// For the interrupt inject path (InjectID non-empty), we still delete the
// pending_injects row at START to prevent duplicate detached runs if the
// pump picks up the same call before we return. If durable enqueue fails, we
// recreate the row so a future tick can retry (P0-2).
func (c *coordinator) startDetachedRun(ctx context.Context, call SessionAgentCall) error {
	// P0-2 fix: delete the pending_injects row at the START to prevent
	// duplicate detached runs. If durable enqueue fails, we'll recreate it.
	if call.InjectID != "" {
		slog.Debug("coordinator: detached run deleting pending_injects row at start",
			"inject_id", call.InjectID)
		if delErr := c.sessions.DeleteInterruptInject(ctx, call.InjectID); delErr != nil {
			slog.Error("coordinator: detached run failed to delete pending_injects row at start",
				"inject_id", call.InjectID, "err", delErr)
			// Continue anyway — row still exists, so duplicates may occur,
			// but this is better than data loss.
		} else {
			slog.Debug("coordinator: detached run deleted pending_injects row at start",
				"inject_id", call.InjectID)
		}
	}

	// P2-1: Generate idempotency key from LogicalCallID (stable per logical request)
	// instead of timestamp (which changes on every retry). Fallback to timestamp
	// with warning if LogicalCallID is empty (should not happen in normal flow).
	// For interrupt inject path, we can use the InjectID as part of the key.
	var idempotencyKey string
	if call.LogicalCallID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.LogicalCallID)
	} else if call.InjectID != "" {
		idempotencyKey = fmt.Sprintf("%s-%s", call.SessionID, call.InjectID)
	} else {
		slog.Warn("coordinator: LogicalCallID is empty, falling back to timestamp-based idempotency key (non-idempotent retries)",
			"session_id", call.SessionID)
		idempotencyKey = fmt.Sprintf("%s-%d", call.SessionID, time.Now().UnixNano())
	}

	// Convert to SessionAgentCallData for serialization
	callData := ToSessionAgentCallData(call)
	callDataJSON, err := json.Marshal(callData)
	if err != nil {
		slog.Error("coordinator: failed to serialize call data for durable enqueue",
			"session_id", call.SessionID, "err", err)
		// For interrupt inject path, recreate the row to prevent data loss
		if call.InjectID != "" && call.ExistingMessageID != "" {
			if recreateErr := c.recreatePendingInjectRow(ctx, call); recreateErr != nil {
				slog.Error("coordinator: also failed to recreate pending_injects row during marshal recovery",
					"inject_id", call.InjectID, "recreate_err", recreateErr, "marshal_err", err)
			}
		}
		return fmt.Errorf("failed to serialize call data for durable enqueue: %w", err)
	}

	// Durably enqueue the call BEFORE returning control (P0-2 requirement)
	// This ensures the call will eventually be executed even if this goroutine exits
	if enqueueErr := c.sessions.EnqueueRunQueueEntry(ctx, idempotencyKey, call.SessionID, callDataJSON); enqueueErr != nil {
		slog.Error("coordinator: failed to durably enqueue call for recovery",
			"session_id", call.SessionID, "err", enqueueErr)
		// For interrupt inject path, recreate the row so a future tick can retry
		if call.InjectID != "" && call.ExistingMessageID != "" {
			if recreateErr := c.recreatePendingInjectRow(ctx, call); recreateErr != nil {
				slog.Error("coordinator: also failed to recreate pending_injects row during enqueue recovery",
					"inject_id", call.InjectID, "recreate_err", recreateErr, "enqueue_err", enqueueErr)
			}
		}
		// For non-inject interrupts, there is no fallback — the call is lost.
		// Return error so the caller can handle it (e.g., surface to HTTP response).
		// For inject interrupts, we've recreated the row so a future tick will retry.
		return fmt.Errorf("failed to durably enqueue call for recovery: %w", enqueueErr)
	}

	slog.Debug("coordinator: durably enqueued call for pump recovery",
		"session_id", call.SessionID, "idempotency_key", idempotencyKey, "inject_id", call.InjectID)
	return nil
}

// recreatePendingInjectRow recreates a pending_injects row for future retry
// (helper for startDetachedRun error path, P0-2 fix). Uses a bounded context
// (WithoutCancel + timeout) to ensure recovery writes have a chance even if
// the calling context is being canceled.
//
// Returns the error from recreation (or nil on success) so the caller can surface it.
func (c *coordinator) recreatePendingInjectRow(originalCtx context.Context, call SessionAgentCall) error {
	// Bounded context: disconnect from caller cancellation but enforce a timeout
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(originalCtx), 10*time.Second)
	defer cancel()

	slog.Debug("coordinator: attempting to recreate pending_injects row",
		"inject_id", call.InjectID, "session_id", call.SessionID, "message_id", call.ExistingMessageID)
	// Get the message content to recreate the row.
	msg, getErr := c.messages.Get(recoveryCtx, call.ExistingMessageID)
	if getErr != nil {
		slog.Error("coordinator: failed to recreate pending_injects row (could not get message)",
			"inject_id", call.InjectID, "message_id", call.ExistingMessageID, "err", getErr)
		return fmt.Errorf("could not get message for recreation: %w", getErr)
	}
	inject := session.PendingInject{
		ID:        uuid.New().String(), // Generate new ID to avoid UNIQUE constraint
		SessionID: call.SessionID,
		MessageID: call.ExistingMessageID,
		Content:   msg.FullText(),
		Interrupt: true,
	}
	createErr := c.sessions.CreatePendingInject(recoveryCtx, inject)
	if createErr != nil {
		slog.Error("coordinator: failed to recreate pending_injects row",
			"inject_id", call.InjectID, "err", createErr)
		return fmt.Errorf("failed to recreate pending_injects row: %w", createErr)
	}
	slog.Info("coordinator: successfully recreated pending_injects row for future retry",
		"new_inject_id", inject.ID, "old_inject_id", call.InjectID)
	return nil
}

// RebuildSessionAgentCall reconstructs a full SessionAgentCall from SessionAgentCallData
// for run queue pump execution (task #340, ROUND 3 migration).
//
// It reconstructs the live Model objects (with fantasy.LanguageModel and CatwalkCfg)
// from the serialized ModelCfg using the coordinator's provider configs and catwalk registry.
// ProviderOptions/Temperature/TopP/TopK/FrequencyPenalty/PresencePenalty are NOT
// reconstructed here — they are pure functions of (Model, ProviderConfig) computed
// via mergeCallOptions during normal execution path.
func (c *coordinator) RebuildSessionAgentCall(ctx context.Context, data session.SessionAgentCallData) (SessionAgentCall, error) {
	var smartModel, fastModel Model
	var err error

	// Determine which models to rebuild using a single atomic snapshot.
	cfg, _ := c.cfg.Snapshot()
	var smartCfg, fastCfg config.SelectedModel
	if data.SmartModel != nil {
		smartCfg = fromSessionModelCfg(*data.SmartModel)
	} else {
		// Use default config for smart model
		smartCfg = cfg.Models[config.SelectedModelTypeSmart]
	}

	if data.FastModel != nil {
		fastCfg = fromSessionModelCfg(*data.FastModel)
	} else {
		// Use default config for fast model
		fastCfg = cfg.Models[config.SelectedModelTypeFast]
	}

	// Build both models (buildModelsFromCfg requires both)
	smartModel, fastModel, err = c.buildModelsFromCfg(ctx, cfg, smartCfg, fastCfg, false)
	if err != nil {
		return SessionAgentCall{}, fmt.Errorf("failed to rebuild models from config: %w", err)
	}

	// cfg here is the SAME atomic snapshot captured above (line ~437) for the
	// smart/fast model rebuild -- NOT a fresh c.cfg.Config() read. A reload
	// landing between the model rebuild above and this provider-options lookup
	// used to be able to hand back provider options from a DIFFERENT config
	// generation than the model itself was built from (task #577/P1-2) -- the
	// entire point of durable recovery is that replaying a call reproduces
	// exactly what was queued, which requires reading provider options from
	// the same generation the model came from.
	//
	// sessionAgent.Run reads ProviderOptions/Temperature/TopP/TopK/FrequencyPenalty/
	// PresencePenalty directly off the call (agent.go's fantasy.AgentStreamCall
	// construction) -- it does NOT recompute them from LargeModel itself. Every
	// other call-site populates these via mergeCallOptions before the call ever
	// reaches Run, so we must do the same here or every durably-recovered call
	// silently loses its provider options and sampling knobs.
	smartProviderCfg, _ := cfg.Providers.Get(smartModel.ModelCfg.Provider)
	providerOptions, temp, topP, topK, freqPenalty, presPenalty := mergeCallOptions(smartModel, smartProviderCfg)

	return SessionAgentCall{
		SessionID:            data.SessionID,
		LogicalCallID:        data.LogicalCallID, // P2-1 fix: restore stable ID
		Prompt:               data.Prompt,
		Attachments:          data.Attachments,
		ProviderOptions:      providerOptions,
		MaxOutputTokens:      data.MaxOutputTokens,
		Temperature:          temp,
		TopP:                 topP,
		TopK:                 topK,
		FrequencyPenalty:     freqPenalty,
		PresencePenalty:      presPenalty,
		NonInteractive:       data.NonInteractive,
		SystemPromptOverride: data.SystemPromptOverride,
		MaxCost:              data.MaxCost,
		MaxTokens:            data.MaxTokens,
		ExistingMessageID:    data.ExistingMessageID,
		InjectID:             data.InjectID,
		SmartModel:           &smartModel,
		FastModel:            &fastModel,
		SystemPromptPrefix:   data.SystemPromptPrefix,
		SystemPrompt:         data.SystemPrompt,
		// Mark as originating from the durable queue so mailbox.submit can
		// skip mb.submitted for this call (P0-1: avoid double-execution).
		// See agent.SessionAgentCall.FromDurableQueue documentation.
		FromDurableQueue: true,
	}, nil
}

// RunSessionAgentCall executes a SessionAgentCall directly for run queue pump execution
// (task #340, ROUND 3 migration). This bypasses the normal buildCall path since the
// call is already fully reconstructed with all necessary data.
func (c *coordinator) RunSessionAgentCall(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
	if err := c.readyWg.Wait(); err != nil {
		return nil, err
	}

	sessionID := call.SessionID

	// Interrupt-inject ticker: watches pending_injects for interrupt=true rows
	// written by `crush sessions inject --interrupt` in another process, and
	// (on the first hit) cancels the running turn and requeues the referenced
	// message so it picks up immediately. Bound to this turn's lifetime via
	// tickerCtx — stopped by the defer as soon as run() returns, so no
	// idle-process polling. The defer ensures the ticker goroutine has joined
	// before RunSessionAgentCall returns.
	tickerCtx, stopTicker := context.WithCancel(ctx)
	tickerDone := c.startInterruptTicker(tickerCtx, sessionID)
	// defers run LIFO: stopTicker (cancel tickerCtx) must fire before we
	// wait on tickerDone, or the join blocks forever waiting for a
	// goroutine that's still parked on <-ctx.Done().
	defer func() {
		stopTicker()
		<-tickerDone
	}()

	return c.currentAgent.Run(ctx, call)
}

// InjectMessage — see Coordinator interface.
func (c *coordinator) InjectMessage(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (message.Message, error) {
	if err := c.readyWg.Wait(); err != nil {
		return message.Message{}, err
	}

	// Resolve the session's model configuration before building the call.
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return message.Message{}, fmt.Errorf("failed to resolve session models for inject: %w", err)
	}

	call, err := c.buildCall(ctx, sessionID, prompt, pinned, attachments)
	if err != nil {
		return message.Message{}, err
	}
	return c.currentAgent.InjectMessage(ctx, call)
}
