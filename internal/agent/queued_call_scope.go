// Queued-call build-ownership retention (Package L, round 2).
//
// An ordinary ExecuteRun call that loses the mailbox race is ACKNOWLEDGED as
// queued, but its pinned toolset keeps only BORROWED construction ownership:
// the tools were built into the sender's build scope (resolveSessionModels →
// pinCallTools → newBatchWorkToolBuild), and the sender's deferred
// callOpts.buildScope.close() (coordinator_run.go) releases that construction
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
// onQueueResolved hook (round-1 flaw B: the hook is a live-waiter marker and
// must never be set on queued calls) — every queue-exit path instead calls
// releaseQueuedBuildOwnership, which closes the marked scope exactly once.
// The arm guard (armQueuedBuildRetention, queued_call_scope.go:86) READS
// call.onQueueResolved solely to SKIP calls whose scope is hook-managed
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
package agent

import (
	"charm.land/fantasy"

	"github.com/google/uuid"
)

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
		ingress, ok := tool.(*batchIngressTool)
		if !ok || ingress.bind != nil || ingress.inv.RegistryID == "" {
			continue
		}
		if _, dup := seen[ingress.inv.RegistryID]; dup {
			continue
		}
		seen[ingress.inv.RegistryID] = struct{}{}
		ids = append(ids, ingress.inv.RegistryID)
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
//   - Calls already carrying onQueueResolved (the InterruptAndSend /
//     runAwaitingAdmission family): they already own a scope through
//     ownDeferredCallScope with a hook-managed lifetime; NOT reusing
//     onQueueResolved for cleanup is the round-2 rule (flaw B), and these
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
	if call == nil || call.IsDrain || call.FromDurableQueue || call.FailIfSessionBusy || call.onQueueResolved != nil || call.CallOptions == nil {
		return
	}
	if call.CallOptions.buildScopeRetained {
		return
	}
	caps := a.diskCapsRef()
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
			scope.close()
			return
		}
		scope.register(func() { caps.release(key) })
	}
	owned := *call.CallOptions
	owned.buildScope = scope
	owned.buildScopeRetained = true
	call.CallOptions = &owned
}

// releaseQueuedBuildOwnership closes one queued call's retained build scope
// exactly once (scope.close is idempotent; a residual-shutdown sweep
// deleting the owner keys underneath turns the later release into a
// no-op delete). No-op for anything that is not a retained queued call.
func releaseQueuedBuildOwnership(call *SessionAgentCall) {
	if call == nil || call.CallOptions == nil || !call.CallOptions.buildScopeRetained {
		return
	}
	call.CallOptions.buildScope.close()
}

// releaseQueuedCallsBuildOwnership applies releaseQueuedBuildOwnership to
// every call in place.
func releaseQueuedCallsBuildOwnership(calls []SessionAgentCall) {
	for i := range calls {
		releaseQueuedBuildOwnership(&calls[i])
	}
}

// releaseTurnEndBuildScope is the dispatcher's single end-of-turn build-scope
// release (called from runOwned's turn loop, agent_run.go). It deliberately
// does NOT depend on origin-options identity: a retained queued call handed
// to a FRESH Run (manual compaction pops it via
// abandonOwnershipAndPopFirstSubmitted and re-runs it, agent_compaction.go)
// arrives as that dispatcher's FIRST call, so originCallOpts equals its own
// copy and the historical origin-inequality test would never fire. The rule
// is therefore: close when the options are not the origin's, OR when they
// carry the queued-retention marker. close is idempotent and exactly-once
// (mutex-guarded, disk_provider_capability.go), and the marker is cleared
// afterwards so a re-queued copy is freshly armed by submit instead of
// keeping a closed scope. No-op for the origin's own first call (its scope
// is closed by the entry-point defers) and for any call carrying
// onQueueResolved (the hook family owns its scope through
// ownDeferredCallScope; the guard necessarily READS the hook here -- it is
// never written or invoked).
func releaseTurnEndBuildScope(call *SessionAgentCall, originCallOpts *CallOptions) {
	if call == nil || call.CallOptions == nil || call.onQueueResolved != nil {
		return
	}
	if call.CallOptions != originCallOpts || call.CallOptions.buildScopeRetained {
		if sc := call.CallOptions.buildScope; sc != nil {
			sc.close()
		}
		call.CallOptions.buildScopeRetained = false
	}
}

// deferReleaseRetainedRunScope implements invariant I1 (Package L round 4):
// a call that carries the retained marker and ENTERS Run is released on
// EVERY return of that Run invocation — success, error, panic, refusal,
// shutdown. The returned func is the deferred release; it closes the scope
// captured AT ENTRY, so it is immune to the caller reassigning its local
// call variable afterwards, and it is a harmless second release after the
// end-of-turn close (scope.close is mutex-guarded idempotent,
// disk_provider_capability.go:97-113).
//
// The sender's own first Run is unaffected: arming happens on a COPY stored
// in mb.submitted (mailbox.submit → armQueuedBuildRetention), so the
// sender's call parameter is never marked and the helper returns a no-op.
// The same holds for RunWithReservedOwnership / runPreReservedOwnership:
// their callers (handleRerunMessage, ReserveExclusive holders) build fresh
// unmarked calls; a queued copy only ever re-enters through the public Run
// (the manual-compaction handoff, agent_compaction.go:295) or through the
// pump's RunSessionAgentCall, whose own unconditional entry defer already
// closes any non-hooked scope (coordinator_run_queue_call.go:354-356).
func deferReleaseRetainedRunScope(call *SessionAgentCall) func() {
	if call == nil || call.CallOptions == nil || !call.CallOptions.buildScopeRetained {
		return func() {}
	}
	scope := call.CallOptions.buildScope
	return func() { scope.close() }
}

// turnEndRelease is the dispatcher's end-of-turn release SITE (invoked from
// runOwned's turn loop, agent_run.go). It is a variable so a test can wrap
// the production release and COUNT that the real call site fired during a
// real Run; every non-test invocation goes to releaseTurnEndBuildScope.
var turnEndRelease = releaseTurnEndBuildScope
