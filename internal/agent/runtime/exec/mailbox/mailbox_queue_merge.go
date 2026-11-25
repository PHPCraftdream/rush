// The mailbox submitted-Queue merge rule (at most one Drain per session),
// moved verbatim out of the agent package root.
package mailbox

import "github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"

// MergeQueuedCall appends call to queued, applying phase-4 step 3's Drain
// merge rule (doc sec.3.4): "at most one Drain call in a session's Queue;
// when it merges with a regular user call, the user call runs and the
// Drain disappears — every turn pulls anyway." A no-op pass-through
// (plain append) for every non-Drain call arriving with no Drain already
// queued, which is every call before step 3 ever produced.
//
//   - An incoming Drain is dropped outright if the Queue is already
//     non-empty: whatever runs next (Drain or not) already pulls at its own
//     turn start, so a second Drain call would only buy a redundant turn.
//   - An incoming non-Drain call drops any Drain already sitting in the
//     Queue: the stronger policy (a real turn) wins, and the real turn's
//     own turn-start pull makes the dropped Drain's job redundant.
func MergeQueuedCall(queued []agentcall.SessionAgentCall, call agentcall.SessionAgentCall) []agentcall.SessionAgentCall {
	if call.IsDrain {
		if len(queued) > 0 {
			return queued
		}
		return append(queued, call)
	}
	if !hasQueuedDrain(queued) {
		return append(queued, call)
	}
	filtered := make([]agentcall.SessionAgentCall, 0, len(queued))
	for _, q := range queued {
		if !q.IsDrain {
			filtered = append(filtered, q)
		}
	}
	return append(filtered, call)
}

// MergeQueuedCallAtHead applies MergeQueuedCall's SAME Drain-merge rule
// (doc sec.3.4: "at most one Drain call in a session's Queue ... on ANY
// insertion path, including returning a displaced call to the head of the
// Queue") for a call that must be reinserted at the HEAD rather than
// appended to the tail -- ReclaimReplacementOrKeep's own case (B17 fix,
// docs/reviews/2026-09-29-async-phase4-round1.md): a call already committed
// as "next to run" before an interrupt's Replacement preempted it goes back
// in front of whatever was already queued, not behind it, but must still
// never let two Drains coexist in the Queue, and must still let an
// incoming real call drop an already-queued Drain.
func MergeQueuedCallAtHead(queued []agentcall.SessionAgentCall, call agentcall.SessionAgentCall) []agentcall.SessionAgentCall {
	if call.IsDrain {
		if len(queued) > 0 {
			// Whatever is already queued already pulls at its own turn
			// start; the displaced Drain would only buy a redundant turn.
			return queued
		}
		return append([]agentcall.SessionAgentCall{call}, queued...)
	}
	if !hasQueuedDrain(queued) {
		return append([]agentcall.SessionAgentCall{call}, queued...)
	}
	filtered := make([]agentcall.SessionAgentCall, 0, len(queued))
	for _, q := range queued {
		if !q.IsDrain {
			filtered = append(filtered, q)
		}
	}
	return append([]agentcall.SessionAgentCall{call}, filtered...)
}

// hasQueuedDrain reports whether queued already holds a Drain call.
func hasQueuedDrain(queued []agentcall.SessionAgentCall) bool {
	for _, q := range queued {
		if q.IsDrain {
			return true
		}
	}
	return false
}
