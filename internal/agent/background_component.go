// Background-job completion notices, Phase-4 autonomous idle-resume, busy
// queries, and the web bg-shell auto-turn cap. Bodies moved from
// coordinator_background.go, coordinator_bgshell_cap.go and bgshell_claim.go
// (the drainComponent/wakeComponent host pattern: the component never holds
// the raw *coordinator, only the backgroundHost port); those files keep the
// thin coordinator delegates.

package agent

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter/subagents"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/installwire"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	mailboxpkg "github.com/PHPCraftdream/rush/internal/agent/runtime/exec/mailbox"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/sessionprompt"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/watchdog"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shellbg"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/google/uuid"
)

// backgroundHost is the narrow coordinator surface the moved background /
// bg-shell-cap bodies need (the wakeComponent pattern).
type backgroundHost interface {
	coordinatorIsNil() bool
	cfg() *config.ConfigStore
	sessions() coresession.Service
	subAgentDrivers() *toolwrap.Registry
	currentAgent() SessionAgent
	notifyWG() *sync.WaitGroup
	notifyCallbackHook() func(context.Context)
	recheckWakes() *sync.WaitGroup
	admitCallback(wg *sync.WaitGroup) (context.Context, bool)
	wakeSession(ctx context.Context, sessionID string, fact bool) error
	noteSubAgentChildRunEnded(sessionID string)
	bgShellRowCancelled(sessionID, shellID string) bool
	addToRecheckSet(sessionID string)
	consecutiveResume(sessionID string) int
	coordinatorBatchCutoverFacts() BatchCutoverFacts
	bgArrivalLock(ctx context.Context) error
	bgArrivalUnlock()
	sessionNoticeStore() *session.SessionNoticeStore
	persistentMode() bool
	autonomyEnabled() bool
	claimAutoResumeSlot(sessionID string, rowID int64, eligible bool) bool
	arb() *arbiter
}

type (
	backgroundComponent       struct{ host backgroundHost }
	coordinatorBackgroundHost struct{ coordinator *coordinator }
)

func newBackgroundComponent(c *coordinator) *backgroundComponent {
	return &backgroundComponent{host: coordinatorBackgroundHost{coordinator: c}}
}

func (h coordinatorBackgroundHost) coordinatorIsNil() bool { return h.coordinator == nil }
func (h coordinatorBackgroundHost) cfg() *config.ConfigStore {
	return h.coordinator.cfg
}

func (h coordinatorBackgroundHost) sessions() coresession.Service {
	return h.coordinator.sessions
}

func (h coordinatorBackgroundHost) subAgentDrivers() *toolwrap.Registry {
	return h.coordinator.subAgentDrivers
}

func (h coordinatorBackgroundHost) currentAgent() SessionAgent {
	return h.coordinator.currentAgent
}
func (h coordinatorBackgroundHost) notifyWG() *sync.WaitGroup { return &h.coordinator.notifyWG }
func (h coordinatorBackgroundHost) notifyCallbackHook() func(context.Context) {
	return h.coordinator.notifyCallbackHook
}

func (h coordinatorBackgroundHost) recheckWakes() *sync.WaitGroup {
	return &h.coordinator.recheckWakes
}

func (h coordinatorBackgroundHost) admitCallback(wg *sync.WaitGroup) (context.Context, bool) {
	return h.coordinator.admitCallback(wg)
}

func (h coordinatorBackgroundHost) wakeSession(ctx context.Context, sessionID string, fact bool) error {
	return h.coordinator.wakeSession(ctx, sessionID, fact)
}

func (h coordinatorBackgroundHost) noteSubAgentChildRunEnded(sessionID string) {
	h.coordinator.noteSubAgentChildRunEnded(sessionID)
}

func (h coordinatorBackgroundHost) bgShellRowCancelled(sessionID, shellID string) bool {
	return h.coordinator.bgShellRowCancelled(sessionID, shellID)
}

func (h coordinatorBackgroundHost) addToRecheckSet(sessionID string) {
	h.coordinator.addToRecheckSet(sessionID)
}

func (h coordinatorBackgroundHost) consecutiveResume(sessionID string) int {
	return h.coordinator.consecutiveResume(sessionID)
}

func (h coordinatorBackgroundHost) coordinatorBatchCutoverFacts() BatchCutoverFacts {
	return coordinatorBatchCutoverFacts(h.coordinator)
}

func (h coordinatorBackgroundHost) bgArrivalLock(ctx context.Context) error {
	return h.coordinator.bgArrival.Lock(ctx)
}
func (h coordinatorBackgroundHost) bgArrivalUnlock() { h.coordinator.bgArrival.Unlock() }
func (h coordinatorBackgroundHost) sessionNoticeStore() *session.SessionNoticeStore {
	return h.coordinator.sessionNoticeStore()
}

func (h coordinatorBackgroundHost) persistentMode() bool {
	return h.coordinator.persistentMode.Load()
}

func (h coordinatorBackgroundHost) autonomyEnabled() bool {
	return h.coordinator.autonomyEnabled()
}

func (h coordinatorBackgroundHost) claimAutoResumeSlot(sessionID string, rowID int64, eligible bool) bool {
	return h.coordinator.claimAutoResumeSlot(sessionID, rowID, eligible)
}
func (h coordinatorBackgroundHost) arb() *arbiter { return &h.coordinator.arb }

// FormatAsyncCompletion makes a completed job visible to the model and UI.
// It preserves the agent API; batchmeta owns the formatting rules.
func FormatAsyncCompletion(completion batchdispatch.AsyncCompletion) string {
	return batchmeta.FormatAsyncCompletion(completion)
}

// notifyAsyncCompletion is the completion wake callback the batch delivery
// invokes: the delivery already committed its durable pending notice
// (a batch row or a session_notices row) --
// there is no text/NoticeKind left to build here (doc sec.3.3: the driver's
// pull reconstructs both from the row at pull time, agent_notice_pull.go).
// This is now just the non-blocking wake HINT (doc sec.3.4): submit a Drain
// call to the session's owning driver iff the committed row wants a wake.
// See wakeSession's own doc for why a driver-owned child session's wake must
// never run on c.host.currentAgent() (task #1049).
func (c *backgroundComponent) notifyAsyncCompletion(completion batchdispatch.AsyncCompletion) {
	// No ctx tagging needed anymore: a Drain call's own constructor
	// (drainctl.NewDrainCall) unconditionally stamps AutoResumed/BackgroundJobNotice/
	// NoticeKind itself, and a Drain never creates a user message (empty
	// Prompt), so there is no Origin left for a caller-supplied ctx to
	// influence either.
	// Supervision progress is recorded when the notice moves into history
	// (agent_notice_pull.go), not here.
	ctx, ok := c.host.admitCallback(c.host.notifyWG())
	if !ok {
		// The terminal fact is already durable; shutdown owes no new wake.
		return
	}
	go func() {
		defer c.host.notifyWG().Done()
		if c.host.notifyCallbackHook() != nil {
			c.host.notifyCallbackHook()(ctx)
		}
		// Re-check trigger (ii)/(iv): this goroutine IS the delivery that
		// the completed job woke -- either directly (mailbox was idle) or
		// merged into an already-live/queued generation (mailbox was busy).
		// Re-evaluate any delegation parked for this session once wakeSession
		// returns, so a release can never land in the window before the
		// wake's effect is visible. wakeSession itself already logs+persists
		// a visible marker on failure; there is nothing else to do with its
		// error here.
		defer c.host.noteSubAgentChildRunEnded(completion.SessionID)
		if completion.Wake {
			_ = c.host.wakeSession(ctx, completion.SessionID, true)
		}
	}()
}

// Phase-4 step 4 (doc sec.3.5): ClaimAsyncCompletions/NextAsyncCompletion/
// HasPendingAsyncJobs and the in-memory ready queue they read are gone.
// `rush run`'s loop (internal/app/app_run_async.go) now claims/releases the
// external-driver marker directly via ClaimExternalDriver/ReleaseExternalDriver
// (coordinator_reaction_source.go) and re-derives its next turn from
// CLIScope against the DB, waiting on WaitForHint (with
// its bounded 5s fallback) instead of draining a memory queue.

// notifyBackgroundJobDone handles an SDK background shell's completion
// (doc sec.2: a completion with no batch root of its own). Step 3: the fact is
// persisted FIRST, durably, as a session_notices row (kind
// NoticeKindBGShellDone, wake=1 per the wake-policy table -- the row's own
// wake bit does not depend on session policy, only whether a TURN is forced
// right now does). AutoResumeOnJobDone decides ONLY that immediate-turn
// question for a session that is not a delegated child, exactly as today: on,
// a Drain call is submitted right away; off, the notice simply waits for the
// next natural turn/Drain to pull it (its wake=1 still counts then). A child
// whose delegation row runs is woken regardless (delegatedChildDriven).
func (c *backgroundComponent) notifyBackgroundJobDone(sessionID string, sh *shellbg.BackgroundShell) {
	// A shell batch_stop already cancelled (its row is terminal, the tool result
	// is its answer) produces no wake notice and takes no auto-resume slot: its
	// exit would otherwise read as "finished: exit 1" and wake the session a
	// second time. The completion hold still ends and the scope is re-checked.
	if c.host.bgShellRowCancelled(sessionID, sh.ID) {
		sh.MarkCompletionRecorded()
		c.host.noteSubAgentChildRunEnded(sessionID)
		return
	}
	stdout, stderr, _, runErr := sh.GetOutput()
	summary := subagents.BackgroundJobSummary(sh.ID, sh.Command, stdout, stderr, shell.ExitCode(runErr), sh.Elapsed(), tools.TruncateOutput)

	// The wake notice and the slot decision are one step (persistBGShellCompletion);
	// the shell's own row is closed by the observer registered at claim time.
	claimed := c.persistBGShellCompletion(sessionID, sh.ID, summary)
	// The notice is durable (or its insert failed and nothing more will be
	// written): the shell's completion hold ends here so the session's own
	// debt answers "is work owed", and only now do this callback's re-checks
	// run (R7B-1). The shell manager releases it on a panic or return too.
	sh.MarkCompletionRecorded()
	// A delegated child is driven by its delegation, not by the auto-resume
	// policy (R6B-1): the claim (web only, AutoResumeOnJobDone on, a free slot)
	// bounds a session's OWN chain of automatic turns. Refusing a child's wake
	// for it left the child's debt owed with nothing to launch it while the
	// parent's delegation waited. drainPermitted stays the single launch gate
	// (a running row allows the turn, a released child is refused there); a root
	// keeps the cap and the policy.
	driven := false
	if !claimed {
		var err error
		if driven, err = c.delegatedChildDriven(context.Background(), sessionID); err != nil {
			slog.Warn("bg-shell completion: delegation state unreadable", "session_id", sessionID, "err", err)
			if !driven {
				c.host.addToRecheckSet(sessionID) // the 60s pass decides it
			}
		}
	}
	if claimed || driven {
		// Autonomous idle-resume: start (or, if busy, queue) a Drain call
		// over the just-persisted notice. The bound was spent by
		// claimAutoResumeSlot (reset by any human message); a driven child spends
		// no slot.
		slog.Info("Phase 4: resuming session on background job completion",
			"session_id", sessionID, "shell_id", sh.ID, "delegated_child", driven,
			"consecutive", c.host.consecutiveResume(sessionID))
		ctx := sessionprompt.WithBackgroundJobNotice(context.Background())
		// The slot was spent ONCE, synchronously, at admission (before the
		// wake's goroutine spawns), so a burst of near-simultaneous
		// completions is bounded deterministically: at most
		// maxConsecutiveAutoResumes completions per human message launch a
		// turn of their own. The launch decision of this wake does not
		// re-check the counter (R2B-16); a paced or refused launch keeps its
		// slot and submits nothing.
		c.host.recheckWakes().Add(1) // waitRecheckWakes covers this detached wake too
		go func() {
			defer c.host.recheckWakes().Done()
			// Re-check trigger (iii): a job owned by this session just
			// became terminal and this goroutine is the delivery it woke.
			// Re-evaluate any delegation parked for this session once
			// wakeSession returns, so the release cannot fire in the window
			// between "job done" and "child claimed its next turn".
			defer c.host.noteSubAgentChildRunEnded(sessionID)
			_ = c.host.wakeSession(ctx, sessionID, true)
		}()
		return
	}

	// AutoResumeOnJobDone off (doc sec.3.4's session-policy table): the
	// notice is already durable and web-visible on the next pull; no turn
	// is forced here, matching today's behavior.
	c.host.noteSubAgentChildRunEnded(sessionID)
}

func (c *backgroundComponent) isBusy() bool {
	return c.host.currentAgent().IsBusy()
}

func (c *backgroundComponent) isSessionBusy(sessionID string) bool {
	return c.host.currentAgent().IsSessionBusy(sessionID)
}

// delegatedChildDriven reports whether sessionID is a delegation's child that
// its delegation currently drives: a RUNNING async_jobs row names it as the
// child. The durable row decides (another process may own the delegation; this
// process's ledger and driver registry can be empty), so a released child, whose
// row is terminal, is not driven even while its driver is still registered.
// An unreadable row falls back to the driver registration, which only ever
// exists for a child, and reports the error: the launch decision that follows
// (wakeSession) is drainPermitted's, which refuses whatever the policy refuses.
func (c *backgroundComponent) delegatedChildDriven(ctx context.Context, sessionID string) (bool, error) {
	return subagents.DelegatedChildDriven(
		ctx,
		sessionID,
		func(ctx context.Context, sessionID string) (string, error) {
			if c.host.sessions() == nil {
				return "", nil
			}
			sess, err := c.host.sessions().Get(ctx, sessionID)
			if errors.Is(err, sql.ErrNoRows) {
				return "", nil
			}
			if err != nil {
				return "", err
			}
			return sess.ParentSessionID, nil
		},
		func(ctx context.Context, parent, child string) (bool, error) {
			return c.host.coordinatorBatchCutoverFacts().LiveDelegationChildOf(ctx, parent, child)
		},
		func(sessionID string) bool {
			_, registered := c.host.subAgentDrivers().Get(sessionID)
			return registered
		},
	)
}

type ctxMutex = subagents.ContextMutex

// bgArrivalInsertedSeam is a test-only hook called between the notice insert and
// the slot decision of persistBGShellCompletion, still under bgArrival.
var bgArrivalInsertedSeam atomic.Pointer[func()]

// bgArrivalEnteredSeam is a test-only hook called with the session id as
// persistBGShellCompletion is entered, before it takes bgArrival.
var bgArrivalEnteredSeam atomic.Pointer[func(sessionID string)]

// readTurnFactsGateSeam is a test-only hook called in readTurnFacts right
// before it takes bgArrival: the seam between the DB half and the arrival
// gate (what an over-cap completion can slip through).
var readTurnFactsGateSeam atomic.Pointer[func()]

// persistBGShellCompletion records a finished background shell and takes (or
// refuses) its auto-resume slot as ONE step relative to the cap check (the
// arbiter cap rule): the check then never sees a row without its slot
// decision. The row's id is what an over-cap completion is remembered by. A
// failed write is logged and the slot decision is made anyway, as before, but
// with no row id: nothing exists to defer, so nothing is recorded as over-cap.
//
// This function writes only the wake channel -- a kind=bg_shell_done
// session_notices row, whose id is what the arbiter's over-cap set is keyed
// by -- and takes the slot decision on it. Fixtures without a bound notice
// store get no row (only the log); the pull drains whatever exists.
func (c *backgroundComponent) persistBGShellCompletion(sessionID, shellID, summary string) (claimed bool) {
	if seam := bgArrivalEnteredSeam.Load(); seam != nil {
		(*seam)(sessionID)
	}
	_ = c.host.bgArrivalLock(context.Background()) // cannot fail: Background never ends
	defer c.host.bgArrivalUnlock()
	// B9-7: job_kill owns a cancelled shell's answer: under the same gate the
	// kill decided at, a callback that lost the race inserts nothing.
	if c.host.bgShellRowCancelled(sessionID, shellID) {
		return false
	}
	var rowID int64
	if store := c.host.sessionNoticeStore(); store != nil {
		c.pruneBGShellOverCap(sessionID)
		insertCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		id, err := store.InsertSessionNoticeReturningID(insertCtx, sessionID, session.NoticeKindBGShellDone, summary, true, "")
		cancel()
		if err != nil {
			slog.Error("failed to persist background-shell-done notice",
				"session_id", sessionID, "shell_id", shellID, "err", err)
		} else {
			rowID = id
		}
	}
	if seam := bgArrivalInsertedSeam.Load(); seam != nil {
		(*seam)()
	}
	eligible := c.host.persistentMode() && c.host.autonomyEnabled()
	return c.host.claimAutoResumeSlot(sessionID, rowID, eligible)
}

// pruneBGShellOverCap is the best-effort prune at a completion's arrival (under
// bgArrival): an unreadable debt leaves the set as it is.
func (c *backgroundComponent) pruneBGShellOverCap(sessionID string) {
	if c.bgShellOverCapCount(sessionID) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), recheckDebtCheckBudget)
	defer cancel()
	rows, err := c.host.coordinatorBatchCutoverFacts().PendingInclusiveDebt(ctx, sessionID)
	if err != nil {
		return
	}
	c.host.arb().pruneOverCap(sessionID, rows.Notices)
}

// bgShellOverCapCount returns how many over-cap completion rows sessionID
// currently remembers (since its last human message).
func (c *backgroundComponent) bgShellOverCapCount(sessionID string) int {
	return c.host.arb().overCapCount(sessionID)
}

// notifyOnBackgroundJobDone mirrors buildTools' kill-switch default
// (coordinator_tools.go): a nil store/options or a nil pointer means enabled.
func (c *backgroundComponent) notifyOnBackgroundJobDone() bool {
	if c.host.coordinatorIsNil() || c.host.cfg() == nil {
		return true
	}
	opts := c.host.cfg().Config().Options
	return opts == nil || opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
}

// Package agent inline-window constant and test seam: the A14 inline-window
// constant, kept here because the batch runtime and the app test harness
// consume it. The seam is REAL: a coordinator carries a disable flag the
// batch tool arming (batch_install_tools.go) resolves through its binding.
// SetInlineWindowForTest zeroes (on=false) or restores (on=true) the
// coordinator's inline window -- the A14 test seam: 0 = the pre-A14
// "started" flow, byte for byte. package app's e2e harnesses disable it
// for scenarios whose scripts match the literal "Async ... started" text or
// count pulled notices; the window-on e2e re-enables it explicitly.
// Returns false for a coordinator with no window to set (test fakes).
func SetInlineWindowForTest(c Coordinator, on bool) bool {
	var set func(bool)
	if s, ok := c.(inlineWindowSetter); ok {
		set = s.setInlineWindowForTest
	}
	return installwire.SetInlineWindowForTest(set, on)
}

type inlineWindowSetter interface {
	setInlineWindowForTest(on bool)
}

// setInlineWindowForTest zeroes (on=false) or restores (on=true) THIS
// coordinator's inline window: the flag is consulted by the batch bash
// arming (inlineWindowDisabledFor), which arms bash detached -- the pre-A14
// "started" flow -- instead of with an escape window.
func (c *coordinator) setInlineWindowForTest(on bool) {
	c.inlineWindowDisabled.Store(!on)
}

// inlineWindowDisabledFor reports whether the coordinator bound to bind has
// its inline window disabled (the SetInlineWindowForTest seam). A build with
// no coordinator in scope (bind nil or unbound fixture) keeps the window.
func inlineWindowDisabledFor(bind *batchCoordinatorBinding) bool {
	return bind != nil && bind.coord != nil && bind.coord.inlineWindowDisabled.Load()
}

// Queued-call build-ownership retention (Package L, round 2).
//
// An ordinary ExecuteRun call that loses the mailbox race is ACKNOWLEDGED as
// queued, but its pinned toolset keeps only BORROWED construction ownership:
// the tools were built into the sender's build scope (resolveSessionModels →
// pinCallTools → newBatchWorkToolBuild), and the sender's deferred
// callOpts.BuildScope.Close() (coordinator_run.go) releases that construction
// as soon as the sender returns — long before the owner dispatcher promotes
// the queued call and executes call.Tools directly (runTurn, agent_turn.go).
// The promotion does not rebuild tools, so the queued turn's first batch
// invocation acquires its root against a pruned construction record and
// fails closed with the ErrDiskProviderUnavailable sentinel family.
//
// Round 2 closes that lifetime gap WITHOUT the round-1 hook: armQueuedBuildRetention
// is invoked ONLY from mailbox.submit's enqueue branch — the moment the
// mailbox accepts the call into submitted — never before the reservation
// decision. A call that wins the reservation, is refused the inter-process
// lock, or fails admission setup never acquires anything, so the round-1
// leak (retained owners taken before the reservation, never released on
// early refusal) is structurally impossible. The armed scope carries NO
// OnQueueResolved hook (round-1 flaw B: the hook is a live-waiter marker and
// must never be set on queued calls) — every queue-exit path instead calls
// releaseQueuedBuildOwnership, which closes the marked scope exactly once.
// The arm guard (armQueuedBuildRetention, queued_call_scope.go:86) READS
// call.OnQueueResolved solely to SKIP calls whose scope is hook-managed
// (the InterruptAndSend / runAwaitingAdmission family); the hook is never
// written or invoked anywhere in this file.
//
// Round 4 states the lifetime invariant explicitly:
//
//	I1: a marked call that ENTERS Run is released on EVERY return of that
//	    Run invocation (success, error, panic, refusal, shutdown) — the
//	    entry defer deferReleaseRetainedRunScope, armed at the top of Run.
//	I2: a marked call that leaves the queue WITHOUT running is released by
//	    the queue-exit paths (clearAll, supersede safety net, orphan handoff
//	    in restartOrphanedWithRetry, hardStop→drainOrReleaseFinal→orphan).
//
// pinnedBatchRegistryIDs returns the distinct pinned batch registry
// identities carried by one call's tool slice: the inv.RegistryID of every
// *batchIngressTool built by the PINNED path (newBatchWorkToolBuild's
// non-deferred branch, bind == nil). Deferred (shared-default-toolset)
// wrappers carry an empty inv.RegistryID and own their construction
// per-dispatch, repairing a prune through holdPinned — they never depended
// on the sender's build scope, so they are skipped.
func pinnedBatchRegistryIDs(list []fantasy.AgentTool) []string {
	ids := make([]string, 0, len(list))
	seen := make(map[string]struct{}, len(list))
	for _, tool := range list {
		ingress, ok := tool.(*toolwrap.BatchIngressTool)
		if !ok || ingress.Bind != nil || ingress.Inv.RegistryID() == "" {
			continue
		}
		if _, dup := seen[ingress.Inv.RegistryID()]; dup {
			continue
		}
		seen[ingress.Inv.RegistryID()] = struct{}{}
		ids = append(ids, ingress.Inv.RegistryID())
	}
	return ids
}

// armQueuedBuildRetention gives one call that is being ACCEPTED INTO THE
// MAILBOX'S SUBMITTED QUEUE its own independently owned build scope: ONE
// additional Construction owner per pinned batch registry in call.Tools,
// released when the scope closes. It is invoked ONLY from mailbox.submit's
// enqueue branch, so a call that wins the reservation, is refused the
// inter-process lock, or fails admission setup NEVER acquires anything --
// the round-1 leak (retained owners taken before the reservation decision,
// never released on early refusal) is structurally impossible here.
//
// No-ops (each deliberate):
//   - Drains: the queue merge rule may drop an incoming Drain without any
//     exit hook; arming one would leak.
//   - FromDurableQueue calls: submit never enqueues them (their durable row
//     is the retry path), so there is nothing to retain for.
//   - FailIfSessionBusy calls: submit refuses them before the enqueue
//     branch; the release helper would never run.
//   - Calls already carrying OnQueueResolved (the InterruptAndSend /
//     runAwaitingAdmission family): they already own a scope through
//     ownDeferredCallScope with a hook-managed lifetime; NOT reusing
//     OnQueueResolved for cleanup is the round-2 rule (flaw B), and these
//     calls do not need a second retained scope.
//   - No bound capability manager, or no pinned batchIngressTool registry
//     in call.Tools: nothing to retain (OFF mode is byte-identical).
//   - An already-marked copy (buildScopeRetained): re-queueing an armed
//     copy must not double-acquire.
//
// Acquisition failure keeps today's fail-closed behavior: the call runs
// and its first batch tool hits the same ErrDiskProviderUnavailable
// sentinel -- never a silent fallback to shared tools or the OS disk.
func (a *sessionAgent) armQueuedBuildRetention(call *SessionAgentCall) {
	newQueuedCallScopeComponent(a).armQueuedBuildRetention(call)
}

type queuedBuildRetentionHost interface {
	diskCapsRef() *DiskProviderRegistry
}

type queuedCallScopeComponent struct {
	host queuedBuildRetentionHost
}

func newQueuedCallScopeComponent(a *sessionAgent) *queuedCallScopeComponent {
	return &queuedCallScopeComponent{host: a}
}

func (q *queuedCallScopeComponent) armQueuedBuildRetention(call *SessionAgentCall) {
	if call == nil || call.IsDrain || call.FromDurableQueue || call.FailIfSessionBusy || call.OnQueueResolved != nil || call.CallOptions == nil {
		return
	}
	if call.CallOptions.BuildScopeRetained {
		return
	}
	caps := q.host.diskCapsRef()
	if caps == nil {
		return
	}
	ids := pinnedBatchRegistryIDs(call.Tools)
	if len(ids) == 0 {
		return
	}
	scope := newBuildResourceScope()
	for _, id := range ids {
		key := diskOwnerKey{kind: diskOwnerConstruction, instance: uuid.NewString(), registryID: id}
		if err := caps.acquire(key); err != nil {
			scope.Close()
			return
		}
		scope.Register(func() { caps.release(key) })
	}
	owned := *call.CallOptions
	owned.BuildScope = scope
	owned.BuildScopeRetained = true
	call.CallOptions = &owned
}

// deferReleaseRetainedRunScope and turnEndRelease are the two names
// agent_run.go calls; their bodies live in runtime/exec/mailbox. They stay
// in the root as one-line delegates because internal/agent/agent_run.go is
// ratcheted at 919 lines in .githooks/file_size_900_allowlist.txt and may
// not grow, so it cannot take the mailbox import these two names need.
func deferReleaseRetainedRunScope(call *SessionAgentCall) func() {
	return mailboxpkg.DeferReleaseRetainedRunScope(call)
}

var turnEndRelease = mailboxpkg.TurnEndRelease

type activeStallClock = drainctl.StallClock

func stallSetPhase(sessionID, kind, name string) { drainctl.StallSetPhase(sessionID, kind, name) }

// stallAbortClosure builds the abort closure armed onto the turn's stall
// clock: it marks the watchdog stalled (so handleStreamFailure takes the
// watchdog-stall branch), stores watchdog.CauseStall through the existing fire path
// (finish part written there), and cancels genCtx to unblock the turn.
func (a *sessionAgent) stallAbortClosure(
	sessionID string,
	watchdogCauseVal *atomic.Int32,
	toolMaxDuration, idleTimeout time.Duration,
	smartModel Model,
	cancel context.CancelFunc,
	forceStall func(),
) func(elapsed time.Duration) {
	return func(elapsed time.Duration) {
		if forceStall != nil {
			forceStall()
		}
		a.handleWatchdogFire(watchdog.CauseStall, elapsed, sessionID, watchdogCauseVal, toolMaxDuration, idleTimeout, smartModel)
		cancel()
	}
}

// stepCallClass and its constants alias the leaf classifier's verdict type
// (runtime/exec/turnstep) for the root guard, reaction chain and their tests.
type stepCallClass = turnstep.StepCallClass

const (
	stepCallAct     = turnstep.StepCallAct
	stepCallRead    = turnstep.StepCallRead
	stepCallReread  = turnstep.StepCallReread
	stepCallWait    = turnstep.StepCallWait
	stepCallRefused = turnstep.StepCallRefused
	stepCallNeutral = turnstep.StepCallNeutral
	stepCallNoWrite = turnstep.StepCallNoWrite
)

// readSpan is a half-open line window [start, end); end == math.MaxInt means
// "to the end of the file". Bounds come from the call's own request, so the
// tail past EOF is an acceptable over-estimate.
type readSpan struct{ start, end int }

func toLeafSpan(sp readSpan) turnstep.ReadSpan {
	return turnstep.ReadSpan{Start: sp.start, End: sp.end}
}

func fromLeafSpan(sp turnstep.ReadSpan) readSpan { return readSpan{start: sp.Start, end: sp.End} }

// readCoverage maps a cleaned file path to the line windows already read in
// this turn. A nil map means "coverage is not tracked at all", which makes
// every windowed read a fresh read.
type readCoverage map[string][]readSpan

// covered reports whether sp lies entirely inside the union of the windows
// recorded for path.
func (c readCoverage) covered(path string, sp readSpan) bool {
	if sp.end <= sp.start {
		return true // an empty window reads nothing
	}
	reach := sp.start
	for _, s := range mergeSpans(c[filepath.Clean(path)]) {
		if s.end <= reach {
			continue
		}
		if s.start > reach {
			return false // a gap opens before sp.end
		}
		reach = s.end
		if reach >= sp.end {
			return true
		}
	}
	return false
}

// add records sp for path, keeping that path's windows sorted and merged.
func (c readCoverage) add(path string, sp readSpan) {
	if c == nil || sp.end <= sp.start {
		return
	}
	key := filepath.Clean(path)
	c[key] = mergeSpans(append(c[key], sp))
}

// clear drops every recorded window (an act, a message insert or a trim all
// mean "the file may have changed").
func (c readCoverage) clear() {
	for key := range c {
		delete(c, key)
	}
}

// snapshot deep-copies the coverage for a concurrent reader (a tool wrapper).
func (c readCoverage) snapshot() readCoverage {
	out := make(readCoverage, len(c))
	for key, spans := range c {
		out[key] = append(make([]readSpan, 0, len(spans)), spans...)
	}
	return out
}

// mergeSpans sorts windows by start and merges overlapping or touching ones in
// a single pass. Files are small, so a copy plus sort beats an interval tree.
func mergeSpans(spans []readSpan) []readSpan {
	if len(spans) < 2 {
		return spans
	}
	out := append(make([]readSpan, 0, len(spans)), spans...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].start != out[j].start {
			return out[i].start < out[j].start
		}
		return out[i].end < out[j].end
	})
	merged := out[:1]
	for _, s := range out[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

func isWaitOnlyCall(call fantasy.ToolCallContent) bool {
	return turnstep.IsWaitOnlyCall(call)
}

// namedSpan is one read window of one file, taken from a call's own input.
type namedSpan struct {
	path string
	span readSpan
}

// readSpans extracts the line windows a view/fs_read call asked for, one per
// item for fs_read's batch form. Any other tool, an unparsable input, or a
// fs_read item the real tool itself would reject yields no window for it --
// the guard does not punish input it cannot read. The parse itself lives in
// the leaf package (turnstep.ReadSpans).
func readSpans(call fantasy.ToolCallContent) []namedSpan {
	leaf := turnstep.ReadSpans(call)
	out := make([]namedSpan, 0, len(leaf))
	for _, named := range leaf {
		out = append(out, namedSpan{path: named.Path, span: fromLeafSpan(named.Span)})
	}
	return out
}

func asyncCapRefused(result fantasy.ToolResultContent) bool {
	return turnstep.AsyncCapRefused(result)
}

// stepClassifyCtx carries the turn-scoped state a classification may consult.
// A zero ctx (nil seen) is the #1113 configuration: reread detection is off
// and every windowed read counts as a fresh read.
type stepClassifyCtx struct {
	seen readCoverage // nil: reread detection impossible; view/fs_read classify as read
}

// seam hands the coverage and the restricted-action table to the leaf
// classifier through its use-point seams; the coverage callback runs at the
// classifier's original readSpans parse site and reads an untracked map
// (nil seen) as fresh windows.
func (cx stepClassifyCtx) seam() turnstep.StepClassifyCtx {
	return turnstep.StepClassifyCtx{
		RestrictedAction: func(toolName string) string { action, _ := toolwrap.RestrictedToolAction(toolName); return action },
		CoveredWindows: func(call fantasy.ToolCallContent) (int, bool) {
			spans := turnstep.ReadSpans(call)
			if len(spans) == 0 {
				return 0, false
			}
			for _, named := range spans {
				if cx.seen == nil || !cx.seen.covered(named.Path, fromLeafSpan(named.Span)) {
					return len(spans), false
				}
			}
			return len(spans), true
		},
	}
}

// classifyStepCall delegates the verdict to the leaf classifier
// (runtime/exec/turnstep.ClassifyStepCall) against this root ctx's seams.
func classifyStepCall(call fantasy.ToolCallContent, result fantasy.ToolResultContent, cx stepClassifyCtx) stepCallClass {
	return turnstep.ClassifyStepCall(call, result, cx.seam())
}
