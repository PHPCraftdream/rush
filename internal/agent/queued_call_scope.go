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
package agent

import (
	"charm.land/fantasy"

	"github.com/google/uuid"

	mailboxpkg "github.com/PHPCraftdream/rush/internal/agent/runtime/exec/mailbox"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
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
