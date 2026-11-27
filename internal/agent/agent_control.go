// The operator-facing control surface: cancel and interrupt-and-replace,
// queueing and inject, busy/queue queries, the runWg admission gate, and
// CancelAll shutdown.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter/delegation"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	mailboxpkg "github.com/PHPCraftdream/rush/internal/agent/runtime/exec/mailbox"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

func (a *sessionAgent) Cancel(sessionID string) {
	// Cancel only the in-flight generation (design §4): a bare interrupt
	// (Ctrl-C, sessions kill, cost/token cap) must NOT discard durable
	// queued user intent. Previously Cancel unconditionally cleared the
	// queue — a latent second bug riding along with P0-2 that silently
	// dropped anything a caller had queued moments earlier via QueueMessage
	// for an unrelated reason. ClearQueue remains the one intentional
	// "drop everything queued" operation. The mailbox (whose current.cancel
	// is populated by beginGeneration in Run's loop and runTurn, and by
	// beginCompact for synchronous compactions) is now the cancel target
	// instead of activeRequests.
	//
	// Falls back to dispatcherCancel when no generation is live yet: Run
	// claims the mailbox (submit stores runCancel as dispatcherCancel) and
	// only calls beginGeneration once it reaches its turn loop, so a Cancel
	// landing in between — while the inter-process OS lock is still being
	// acquired — would otherwise find current.cancel nil and silently
	// no-op. Before the mailbox migration tryReserveSession wrote runCancel
	// straight into activeRequests, so that window WAS covered; keeping the
	// fallback preserves it (the task #206 "Cancel must never find a dead
	// placeholder" invariant).
	mb := a.getMailbox(sessionID)
	mb.Mu.Lock()
	genCancel := mb.Current.Cancel
	if genCancel == nil {
		genCancel = mb.DispatcherCancel
	}
	mb.Mu.Unlock()
	if genCancel != nil {
		slog.Debug("Request cancellation initiated", "session_id", sessionID)
		genCancel()
	}
}

func (a *sessionAgent) ClearQueue(sessionID string) {
	// The single intentional "drop everything queued" operation (design
	// §4): clears the mailbox's submitted/replacement/injects atomically
	// under its lock. Cancel no longer touches any of these; only this
	// method does.
	slog.Debug("Clearing queued prompts", "session_id", sessionID)
	a.getMailbox(sessionID).ClearAll()
}

func (a *sessionAgent) QueueMessage(call SessionAgentCall) {
	a.getMailbox(call.SessionID).Queue(call)
}

// InterruptAndReplace is the coordinator's single entry point for "interrupt
// and replace" (design §4), replacing the QueueMessage+Cancel two-step that
// P0-2 made self-defeating: Cancel deterministically wiped the very message
// QueueMessage had just queued the line before. It atomically records call
// as the replacement the current owner must run next, and cancels ONLY the
// in-flight generation — leaving the dispatcher (Run's turn loop) alive to
// drain the replacement via drainAfterCancel. Returns true when a turn was
// actually interrupted; false when the session was idle (nothing to cancel —
// the caller should queue call for the next Run itself).
func (a *sessionAgent) InterruptAndReplace(sessionID string, call SessionAgentCall) bool {
	cancelFn, hadOwner := a.getMailbox(sessionID).InterruptAndReplace(call)
	if !hadOwner {
		return false
	}
	if cancelFn != nil {
		cancelFn()
	}
	return true
}

// ActiveCall returns the current in-process call when a generation is live.
// Coordinator control paths use this to preserve non-serializable options
// during an interrupt; ordinary queueing never exposes the call.
func (a *sessionAgent) ActiveCall(sessionID string) (SessionAgentCall, bool) {
	return a.getMailbox(sessionID).CurrentCallSnapshot()
}

// ActiveCallState reports ownership separately from active-call publication.
// An owned mailbox with no published snapshot is a retryable admission window,
// not permission to route an interrupt through the durable queue.
func (a *sessionAgent) ActiveCallState(sessionID string) (SessionAgentCall, bool, bool) {
	return a.getMailbox(sessionID).CurrentCallState()
}

// ActiveCallStateWithToken is the internal compare-and-act snapshot used by
// coordinator interrupt delivery. ActiveCallState remains unchanged for
// compatibility with existing control implementations and test doubles.
func (a *sessionAgent) ActiveCallStateWithToken(sessionID string) (SessionAgentCall, mailboxpkg.ActiveCallToken, bool, bool) {
	return a.getMailbox(sessionID).CurrentCallStateWithToken()
}

// InterruptAndReplaceIfCurrent delivers an interrupt only when token still
// identifies the mailbox's current published generation. It returns false for
// an owner or generation transition without touching the newer call.
func (a *sessionAgent) InterruptAndReplaceIfCurrent(sessionID string, call SessionAgentCall, token mailboxpkg.ActiveCallToken) bool {
	cancelFn, matched := a.getMailbox(sessionID).InterruptAndReplaceIfCurrent(token, call)
	if !matched {
		return false
	}
	if cancelFn != nil {
		cancelFn()
	}
	return true
}

// InjectMessage — see SessionAgent interface comment. Persists immediately
// (UI updates via the same pubsub path that handleSendMessage uses) and, if
// the session is currently running, atomically queues the persisted row into
// the mailbox's injects list (stamped with the current generation id) so the
// next PrepareStep splices it into prepared.Messages without duplicating the
// DB write. The atomic busy-check + inject (mailbox.injectIfBusy, design §5
// stage 2.4) replaces the old non-atomic IsSessionBusy + injectQueue.Append
// pair.
func (a *sessionAgent) InjectMessage(ctx context.Context, call SessionAgentCall) (message.Message, error) {
	msg, err := a.createUserMessage(ctx, call)
	if err != nil {
		return message.Message{}, err
	}
	// Atomic busy-check + inject under one mb.Mu hold (design §5, stage 2.4):
	// replaces the non-atomic IsSessionBusy + injectQueue.Append pair that
	// had a window between check and append where the owner could finish and
	// release to mbIdle. When the session is idle, the message lives only in
	// the DB and is picked up by the next Run's preamble naturally.
	a.getMailbox(call.SessionID).InjectIfBusy(msg)
	return msg, nil
}

// tryAdmitRunWg atomically checks shuttingDown and registers one unit of
// work in runWg — see admitMu's doc for why the check and the Add must be
// one operation relative to CancelAll's own critical section (closes a
// real "Add concurrently with Wait" race, P1-1). Returns false (does NOT
// call Add) if shutdown has already begun.
func (a *sessionAgent) tryAdmitRunWg() bool {
	a.admitMu.Lock()
	defer a.admitMu.Unlock()
	if a.shuttingDown.Load() {
		return false
	}
	a.runWg.Add(1)
	return true
}

// ShuttingDown reports whether CancelAll has latched this agent's
// shutdown latch.
func (a *sessionAgent) ShuttingDown() bool {
	return a.shuttingDown.Load()
}

func (a *sessionAgent) CancelAll() (stillBusy bool) {
	// Refuse all FUTURE Run() calls before touching anything else (closing
	// review, blocker 1). This must come first and must live on the agent
	// rather than the mailboxes: the sweep below can only latch mailboxes
	// that already exist, and mailboxes are created lazily, so a Run for a
	// session id the sweep never saw would otherwise get a fresh mailbox
	// with stopped == false and run a full turn nothing will cancel.
	a.admitMu.Lock()
	a.shuttingDown.Store(true)
	a.admitMu.Unlock()

	// Latch EVERY mailbox closed FIRST, before cancelling anything (round
	// 14 review, P0-C). Ordering is what makes the shutdown terminal: a
	// turn cancelled below immediately runs its cancel-handling branch,
	// and that branch drains replacement/submitted and starts the NEXT
	// turn. With `stopped` already latched, every drain refuses instead —
	// so no new provider request can begin while we are trying to exit.
	//
	// This is also why it hard-stops rather than reusing Cancel(): Cancel
	// deliberately targets only the current generation, leaving the
	// dispatcher (runCancel, the whole-Run() context) alive precisely so a
	// turn loop survives to run a replacement. Shutdown needs the opposite,
	// so hardStop hands back the DISPATCHER cancel too and we fire it.
	//
	// Latching is unconditional rather than gated on the mailbox currently
	// being owned: a Run() sitting between turns, or one that reaches its
	// drain in the window between this sweep and process exit, must be
	// refused as well.
	for _, mb := range a.mailboxes.Seq2() {
		dispatcherCancel, genCancel := mb.HardStop()
		// Generation first, then dispatcher: the generation cancel is what
		// promptly unblocks an in-flight provider stream, and cancelling
		// the dispatcher afterwards tears down the Run() that owns it.
		// Both are invoked outside mb.Mu — hardStop has already returned.
		if genCancel != nil {
			genCancel()
		}
		if dispatcherCancel != nil {
			dispatcherCancel()
		}
	}

	a.CacheKeepAliveComponent.CancelAll()

	// Stop every in-flight title-generation goroutine too. Their contexts
	// are deliberately detached from each turn's own cancel (see
	// startTitleGeneration), so without this a title provider that ignores
	// cancellation would hold runWg — and with it the wait below, and
	// App.Shutdown — for the whole grace. Fired after the mailbox sweep so
	// the turn's own unwind has already been triggered.
	a.titleGens.cancelAll()

	// Wait for all active Run() goroutines to finish. This provides a true
	// join primitive instead of the old IsBusy() polling, which could report
	// "not busy" before the actual Run() goroutines had unwound (defer
	// cleanup, final DB writes, etc.). Use a 5-second timeout to match the
	// old grace period.
	grace := DefaultCancelAllGrace
	if a.cancelAllGrace > 0 {
		grace = a.cancelAllGrace
	}
	waitDone := make(chan struct{})
	go func() {
		a.runWg.Wait()
		close(waitDone)
	}()

	select {
	case <-waitDone:
		// All Run() goroutines have finished. Clean shutdown.
		return false
	case <-time.After(grace):
		// Grace period expired but some Run() goroutines are still running.
		// Return true to signal forced shutdown.
		return true
	}
}

// IsBusy reports whether ANY session this agent knows about currently has a
// live owner. Task #206-followup (round 9 review, BLOCKER-1): this used to
// read activeRequests directly, but releaseSessionReservation (mailbox.
// drainOrRelease) stopped clearing the plain-sessionID activeRequests entry
// once the mailbox migration (P0-3, task #282) landed — tryReserveSession/
// Run's loop still WRITE it every turn (tryReserveSession in
// agent_run.go, runTurn in agent_turn.go) via
// activeRequests.Set, so after the FIRST turn any session ever ran,
// activeRequests permanently holds a non-nil (already-fired, inert)
// cancelFunc for it. The old activeRequests-based IsBusy therefore returned
// true forever after any session's first turn completed, which meant
// CancelAll's 5-second drain loop (App.Shutdown, reached by every `rush
// run` via `defer a.Shutdown()`) always ran to its full timeout instead of
// returning immediately once genuinely idle. mailboxes.State is the
// post-migration source of truth for "does this session have a live
// owner" (see IsSessionBusy's doc) and is correctly reset to mbIdle on
// release, so it does not have this staleness problem.
//
// `mb.State != mailboxpkg.MbIdle` (NOT `mb.State == mbOwned`) is deliberate as of
// #296/P1-C: mbReleasing means drainOrReleaseFinal's release() — the OS
// session-lock teardown — is running with mb.Mu NOT held, on some OTHER
// goroutine than this one. If IsBusy() treated mbReleasing as "not busy",
// CancelAll's 5-second drain loop could see every mailbox as idle and
// return WHILE that release() disk I/O (and the whole-process DB teardown
// App.Shutdown runs right after CancelAll returns) is still in flight —
// reopening the exact class of race HIGH-1 closed, just observed through
// the shutdown path instead of a same-process "become the new owner" path.
func (a *sessionAgent) IsBusy() bool {
	for _, mb := range a.mailboxes.Seq2() {
		mb.Mu.Lock()
		busy := mb.State != mailboxpkg.MbIdle
		mb.Mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

// IsSessionBusy reports whether sessionID currently has an owner (design §2's
// mapping table: mailbox.State != mbIdle replaces the old
// activeRequests.Get(sessionID) busy check). This is now the ONLY source of
// truth for the main-session busy St: releaseSessionReservation (via
// mailbox.DrainOrRelease) no longer touches activeRequests at all, so
// activeRequests entries for a plain sessionID key would otherwise never be
// cleared. activeRequests itself is untouched by this migration and remains
// the cancel-target lookup Cancel/CancelAll/the peak-hours abort path use —
// call-site migration happens incrementally, one piece at a time (see the
// design doc's §7 migration plan).
func (a *sessionAgent) IsSessionBusy(sessionID string) bool {
	mb := a.getMailbox(sessionID)
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	return mb.State != mailboxpkg.MbIdle
}

// QueuedPrompts reports how many calls are waiting for sessionID's current
// owner to finish, in the mailbox's submitted queue. All queue paths
// (QueueMessage, submit during busy session, abandonOwnership survivors)
// now go through the mailbox's submitted queue as the single source of
// truth.
func (a *sessionAgent) QueuedPrompts(sessionID string) int {
	mb := a.getMailbox(sessionID)
	mb.Mu.Lock()
	defer mb.Mu.Unlock()
	return len(mb.Submitted)
}

// QueuedPromptsList is QueuedPrompts' list counterpart — see its doc. A
// queued Drain call (phase-4 step 3) is never listed: it carries no prompt
// text of its own and is not user-facing queued work.
func (a *sessionAgent) QueuedPromptsList(sessionID string) []string {
	mb := a.getMailbox(sessionID)
	mb.Mu.Lock()
	mailboxCalls := append([]SessionAgentCall(nil), mb.Submitted...)
	mb.Mu.Unlock()

	if len(mailboxCalls) == 0 {
		return nil
	}
	var prompts []string
	for _, call := range mailboxCalls {
		if call.IsDrain {
			continue
		}
		prompts = append(prompts, call.Prompt)
	}
	return prompts
}

// The Drain call kind (phase-4 step 3, docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.4): a SessionAgentCall whose only job is the
// driver's notice pull (agent_notice_pull.go) at turn start, followed by an
// empty-prompt reaction turn if (and only if) debt that pull made visible is
// still owed and the ONE launch predicate (drainPermitted, see
// decideDrainTurn in agent_drain_decision.go) allows a turn. Submitted
// through the ordinary Run/submit path: idle -> becomes the owner like any
// call; busy -> queued, merging with whatever is already there
// (mailbox_ownership.go's submit).
// drainCallFor builds sessionID's Drain call. Slot-derived child drivers
// resolve their live role slot per new automatic turn; explicit/non-slot
// drivers keep the frozen model. Root sessions resolve their own pins as usual.
// R12-H2b §2.3: both branches resolve the session's execution fence
// FIRST — the returned preparedContinuation holds the Continuation owner
// across the dispatch window (the caller releases it after Run returns),
// and its requirement is stamped onto the drain call (and, on the root
// branch, onto the ctx so the pinned toolset is provider-backed). The root
// branch cleans up after itself on every failure: a defer releases the
// Continuation owner and closes the build scope unless the ownership was
// transferred to the successful return. A lost binding fails closed typed
// before anything runs.
func (c *coordinator) drainCallFor(ctx context.Context, sessionID string) (SessionAgentCall, *preparedContinuation, error) {
	var rootLogicalID string
	var rootRoleContract *session.RoleContract
	if _, ok := c.subAgentDrivers.Get(sessionID); !ok {
		rootCall := SessionAgentCall{SessionID: sessionID}
		if store, supported := c.sessions.(session.SessionRootRoleContractStore); supported {
			logicalID, contract, found, err := store.GetRootRoleContract(ctx, sessionID)
			if err != nil {
				return SessionAgentCall{}, nil, err
			}
			if found && logicalID != "" {
				rootLogicalID = logicalID
				rootRoleContract = contract.Clone()
				rootCall.LogicalCallID = logicalID
				if contract != nil {
					rootCall.CallOptions = &CallOptions{RoleContract: contract}
				}
			}
		}
		var restoreErr error
		rootCall, restoreErr = delegation.RestoreRootRoleContract(ctx, c.sessions, rootCall)
		if restoreErr != nil {
			return SessionAgentCall{}, nil, restoreErr
		}
		if rootCall.CallOptions != nil && rootCall.CallOptions.RoleContract != nil {
			ctx = WithCallOptions(ctx, rootCall.CallOptions)
		}
	}
	if _, ok := c.subAgentDrivers.Get(sessionID); !ok && c.drainCredentialBoundary(ctx, sessionID) {
		return SessionAgentCall{}, nil, fmt.Errorf("%w: session %s", diskport.ErrCredentialBindingUnavailable, sessionID)
	}
	if driver, ok := c.subAgentDrivers.Get(sessionID); ok {
		// §6.2 (preserved from the old wakeNoticeCall driver branch): every
		// wake re-inherits the driver's parentSessionID's allowlist baseline
		// onto the child BEFORE waking it -- the child's own turn no longer
		// clears its allowlist entry on return, so re-arming here keeps a
		// woken turn judged by the delegation's policy instead of falling
		// back to the process-wide gate.
		if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok && driver.ParentSessionID != "" {
			mgr.InheritSessionRunAllowlistForGeneration(driver.ParentSessionID, sessionID, driver.Generation)
		}
		call := drainctl.NewDrainCall(driver.CallFor(""))
		if driver.Call.RoleContract != nil && (call.CallOptions == nil || call.CallOptions.RoleContract == nil) {
			call.CallOptions = WithRoleContractCopy(call.CallOptions, driver.Call.RoleContract)
		}
		if call.RequestedModelRole != "" {
			var roleErr error
			call, roleErr = c.refreshRequestedDelegationCall(ctx, driver.ParentSessionID, call)
			if roleErr != nil {
				return SessionAgentCall{}, nil, roleErr
			}
		} else if driver.SlotRole != "" && call.Credentials == nil {
			if refreshed, providerCfg, ok := c.refreshDrainModel(ctx, driver.SlotRole); ok {
				call = drainctl.ApplyDrainModelOptions(call, refreshed)
				call.ProviderOptions = modelkit.GetProviderOptions(sessionID, refreshed, providerCfg)
			}
		}
		c.freshenDrainClient(ctx, &call)
		prep, prepErr := c.PrepareContinuation(ctx, sessionID, requirementFromCall(call))
		if prepErr != nil {
			return SessionAgentCall{}, nil, prepErr
		}
		prep.applyTo(&call)
		return call, prep, nil
	}
	// Root branch: resolve the fence BEFORE model resolution so the pinned
	// toolset is built with the provider already on the ctx (R12-H2b §2.3).
	// Failure cleanup (gh#10b): every error return below that does not hand
	// the Continuation owner to a caller must release it (and close the
	// build scope it registered pins on) — the deferred abandon below runs
	// unless the ownership is transferred at the final return.
	prep, prepErr := c.PrepareContinuation(ctx, sessionID, nil)
	if prepErr != nil {
		return SessionAgentCall{}, nil, prepErr
	}
	var capOpts *CallOptions
	transferred := false
	defer func() {
		if !transferred && prep != nil {
			if capOpts != nil {
				capOpts.BuildScope.Close()
			}
			prep.release()
		}
	}()
	if err := c.refuseLostRequestedRoleDriver(ctx, sessionID); err != nil {
		return SessionAgentCall{}, nil, err
	}
	if prep != nil {
		capOpts = &CallOptions{}
		prep.applyToOptions(capOpts)
		runSpec, policyErr := prep.restorePolicyBaseline(ctx, sessionID, capOpts)
		if policyErr != nil {
			return SessionAgentCall{}, nil, policyErr
		}
		ctx = WithCallOptions(ctx, capOpts)
		if rootRoleContract != nil {
			capOpts.RoleContract = rootRoleContract.Clone()
		}
		// CallOptions remain the source of call policy; the marker now reflects
		// the latest persisted user-message origin rather than inherited setup.
		origin, hasUserMessage := drainctl.LastUserMessageOrigin(ctx, c.messages, sessionID)
		capOpts.WebTurnOptIn = hasUserMessage && origin == message.OriginWeb && capOpts.ModelRole != config.SelectedModelTypeReviewer
		ctx = WithCallOptions(ctx, capOpts)
		// Carry the fence's folder-scope spec on the ctx too, mirroring
		// ExecuteRun's per-call arming: its presence is what lets the
		// durable queue re-serialize the scope.
		if prep.requirement.FolderScopeSpec != nil {
			ctx = WithFolderScopeSpec(ctx, fromSessionFolderScopeSpec(prep.requirement.FolderScopeSpec))
		}
		// Arm the fence's restricted-run policy in both forms: compiled for
		// this turn's enforcement, spec for the durable re-serialization.
		// Degrade-on-pattern-error, like the rebuild (T12): invalid patterns
		// are dropped with a warning and the compiled allowlist stays
		// restricted — the spec itself is still armed verbatim.
		if runSpec != nil {
			compiled, compileErr := permission.BuildRunAllowlist(*runSpec)
			if compileErr != nil {
				slog.Warn("drainCallFor: dropped invalid restricted-run patterns from the fence policy spec", "session_id", sessionID, "err", compileErr)
			}
			ctx = WithRunAllowlist(ctx, &compiled)
			ctx = WithRunAllowlistSpec(ctx, runSpec)
		}
	}
	if prep == nil {
		// With no execution fence, use CallOptions to carry the web-turn policy
		// into model resolution without replacing unrelated context options.
		origin, hasUserMessage := drainctl.LastUserMessageOrigin(ctx, c.messages, sessionID)
		if existing := callOptionsFrom(ctx); existing != nil || hasUserMessage && origin == message.OriginWeb {
			var opts CallOptions
			if existing != nil {
				opts = *existing
			}
			opts.WebTurnOptIn = hasUserMessage && origin == message.OriginWeb && opts.ModelRole != config.SelectedModelTypeReviewer
			ctx = WithCallOptions(ctx, &opts)
		}
	}
	if rootRoleContract != nil {
		ctx = WithCallRoleContract(ctx, rootRoleContract)
	}
	pinned, err := c.resolveSessionModels(ctx, sessionID)
	if err != nil {
		return SessionAgentCall{}, nil, err
	}
	// A reaction after a long job may find the OAuth token expired: refresh it
	// BEFORE the call is built (the web and child Drains bypass runInternal,
	// where the CLI refreshes), and resolve the models again so the call
	// carries the new client. A failed refresh proceeds with the token we have.
	if refreshed, refreshErr := c.refreshExpiredToken(ctx, pinned.providerCfg); refreshErr != nil {
		slog.Warn("Drain: OAuth token refresh failed; proceeding with the existing token", "session_id", sessionID, "error", refreshErr)
	} else if refreshed {
		if pinned, err = c.resolveSessionModels(ctx, sessionID); err != nil {
			return SessionAgentCall{}, nil, err
		}
	}
	call, err := c.buildCall(ctx, sessionID, "", pinned, nil)
	if err != nil {
		return SessionAgentCall{}, nil, err
	}
	call = drainctl.NewDrainCall(call)
	if rootLogicalID != "" {
		call.LogicalCallID = rootLogicalID
	}

	call.RoleContract = agentcall.CloneRoleContractForCall(rootRoleContract)
	call.CallOptions = func() *CallOptions {
		if opts := callOptionsFrom(ctx); opts != nil {
			copy := *opts
			copy.RoleContract = agentcall.CloneRoleContractForCall(rootRoleContract)
			return &copy
		}
		return &CallOptions{RoleContract: agentcall.CloneRoleContractForCall(rootRoleContract)}
	}()
	// Idempotent stamping: keeps DiskRequirement on the struct itself even
	// though the ctx-carried options already carry it (R12-H2b §2.3).
	prep.applyTo(&call)
	// The Continuation owner now belongs to the caller, which releases it
	// after Run returns; the deferred abandon must not fire.
	transferred = true
	return call, prep, nil
}

// freshenDrainClient brings a delegated child's Drain call onto the current
// refreshable credentials (an OAuth token or an API-key template). The child's
// driver template froze its model client when the delegation started, so after
// a long job (or a refresh by any other caller: the 401 accounting refreshes an
// API-key template and publishes the new key) it carries an old token or key:
// refresh an expired OAuth token, then rebuild the pinned model's client from
// the current config (same model, new client). Best effort: a failure leaves the
// template's client, and a 401 then takes the counted, paced path of the
// accounting. A provider with no refreshable credential keeps its client, and
// per-call credentials (tenant keys) are never replaced with the shared
// config's.
func (c *coordinator) freshenDrainClient(ctx context.Context, call *SessionAgentCall) {
	if c.cfg == nil || call.SmartModel == nil || call.Credentials != nil {
		return
	}
	providerID := call.SmartModel.ModelCfg.Provider
	_, providerCfg, err := c.rebuildInputs(providerID)
	if err != nil || !hasRefreshableCredential(providerCfg) {
		return
	}
	if _, err := c.refreshExpiredToken(ctx, providerCfg); err != nil {
		slog.Warn("Drain: OAuth token refresh failed; proceeding with the existing token", "session_id", call.SessionID, "provider", providerID, "error", err)
		return
	}
	fresh, err := c.rebuildOnCurrentCredentials(ctx, *call.SmartModel, true)
	if err != nil {
		slog.Warn("Drain: could not rebuild the child's model client", "session_id", call.SessionID, "provider", providerID, "error", err)
		return
	}
	call.SmartModel = &fresh
}
