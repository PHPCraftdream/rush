// The mailbox release helpers: panic-safe release invocation, the retained
// queued-call build-scope release, and the end-of-turn release site.
package mailbox

import (
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
)

// CallReleaseRecoveringPanic invokes release and recovers any panic it
// raises, folding it into the returned error instead of letting it unwind
// through drainOrReleaseFinal. #296/P1-C moved release() (real disk I/O:
// Truncate/Seek/Sync/sidecar-unlink/unlock/Close) outside mb.mu specifically
// so a slow filesystem cannot stall the control plane; a panic escaping
// uncaught would be worse than the stall it replaces — it would leave
// mb.state stuck at mbReleasing forever (mb.mu is not even held at the panic
// site to recover under), permanently reporting the session busy with no
// path back to mbIdle. Isolated into its own function (rather than an
// inline func(){}() in the caller) so the defer/recover pair has a single,
// obviously-correct home.
func CallReleaseRecoveringPanic(release func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("mailbox release callback panicked: %v", r)
		}
	}()
	return release()
}

// ReleaseQueuedBuildOwnership closes one queued call's retained build scope
// exactly once (scope.close is idempotent; a residual-shutdown sweep
// deleting the owner keys underneath turns the later release into a
// no-op delete). No-op for anything that is not a retained queued call.
func ReleaseQueuedBuildOwnership(call *agentcall.SessionAgentCall) {
	if call == nil || call.CallOptions == nil || !call.CallOptions.BuildScopeRetained {
		return
	}
	call.CallOptions.BuildScope.Close()
}

// ReleaseQueuedCallsBuildOwnership applies ReleaseQueuedBuildOwnership to
// every call in place.
func ReleaseQueuedCallsBuildOwnership(calls []agentcall.SessionAgentCall) {
	for i := range calls {
		ReleaseQueuedBuildOwnership(&calls[i])
	}
}

// ReleaseTurnEndBuildScope is the dispatcher's single end-of-turn build-scope
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
// OnQueueResolved (the hook family owns its scope through
// ownDeferredCallScope; the guard necessarily READS the hook here -- it is
// never written or invoked).
func ReleaseTurnEndBuildScope(call *agentcall.SessionAgentCall, originCallOpts *agentcall.CallOptions) {
	if call == nil || call.CallOptions == nil || call.OnQueueResolved != nil {
		return
	}
	if call.CallOptions != originCallOpts || call.CallOptions.BuildScopeRetained {
		if sc := call.CallOptions.BuildScope; sc != nil {
			sc.Close()
		}
		call.CallOptions.BuildScopeRetained = false
	}
}

// DeferReleaseRetainedRunScope implements invariant I1 (Package L round 4):
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
func DeferReleaseRetainedRunScope(call *agentcall.SessionAgentCall) func() {
	if call == nil || call.CallOptions == nil || !call.CallOptions.BuildScopeRetained {
		return func() {}
	}
	scope := call.CallOptions.BuildScope
	return func() { scope.Close() }
}

// TurnEndRelease is the dispatcher's end-of-turn release SITE (invoked from
// runOwned's turn loop, agent_run.go). It is a variable so a test can wrap
// the production release and COUNT that the real call site fired during a
// real Run; every non-test invocation goes to ReleaseTurnEndBuildScope.
var TurnEndRelease = ReleaseTurnEndBuildScope
