// A SessionAgentCall carrying OnQueueResolved (coordinator.runAwaitingAdmission,
// #1036) has an in-process waiter blocked on that hook firing -- no implicit
// timeout exists anywhere in this call chain, so a call that is silently
// dropped instead of resolved leaves its waiter blocked forever. This file is
// the single place every "this call left the mailbox's queue without ever
// becoming a turn" path routes through to guarantee that never happens: the
// orphan/restart durable-enqueue path (restartOrphanedWithRetry -- durable
// enqueue serializes the call via ToSessionAgentCallData, which has no field
// for OnQueueResolved (json:"-"), so a pump-rebuilt replay could never call it
// back), ClearQueue (mailbox.clearAll), and interruptAndReplace's replacement
// slot being overwritten before the call it held ever ran.
package mailbox

import (
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
)

// ErrQueuedTurnNotRun is the sentinel every such resolution wraps. Callers
// blocked in runAwaitingAdmission see errors.Is(err, ErrQueuedTurnNotRun) to
// distinguish "the queued turn was abandoned before it ran" from an actual
// provider/turn error.
var ErrQueuedTurnNotRun = errors.New("queued turn was not run")

// ResolveCallWithError invokes call's OnQueueResolved hook, if any, with an
// error wrapping ErrQueuedTurnNotRun and reason. No-op for a call with no
// in-process waiter. Safe to call more than once for the same call (the hook
// wrapper installed by runAwaitingAdmission is itself exactly-once-guarded),
// but every call site here is already structured to invoke it at most once
// per call, since a call leaves the queue exactly once.
func ResolveCallWithError(call agentcall.SessionAgentCall, reason string) {
	if call.OnQueueResolved != nil {
		call.OnQueueResolved(nil, fmt.Errorf("%w: %s", ErrQueuedTurnNotRun, reason))
	}
}

// ResolveCallsWithError applies ResolveCallWithError to every call in calls.
func ResolveCallsWithError(calls []agentcall.SessionAgentCall, reason string) {
	for _, call := range calls {
		ResolveCallWithError(call, reason)
	}
}

// SplitCallsWithoutWaiters resolves (with reason) every call in calls that
// carries an in-process waiter (OnQueueResolved != nil) and returns the
// remaining calls -- the ones actually safe to durably enqueue, since they
// have nothing in-process depending on their outcome. Used by
// restartOrphanedWithRetry so a call with a live waiter is NEVER durably
// enqueued (its hook would be unreachable once serialized) and never merely
// dropped either.
func SplitCallsWithoutWaiters(calls []agentcall.SessionAgentCall, reason string) []agentcall.SessionAgentCall {
	if len(calls) == 0 {
		return calls
	}
	kept := calls[:0]
	for _, call := range calls {
		if call.OnQueueResolved != nil {
			ResolveCallWithError(call, reason)
			continue
		}
		kept = append(kept, call)
	}
	return kept
}
