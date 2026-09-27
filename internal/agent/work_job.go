// Single work-ledger job record (phase 1 of the async structured-concurrency
// rewrite, docs/plans/2026-09-27-async-structured-concurrency.md). Replaces
// asyncJobState (former async_job_registry.go) and subAgentOutcomeEntry
// (former subagent_outcome.go) with one type: a delegation (`agent`/
// `agentic_fetch`) used to have TWO records across two registries for one
// call's whole lifecycle; here there is one, from Start to delivery.
//
// Deliberately NOT in this type (docs/plans/2026-09-27-async-phase1-spec.md
// §0.1.4/§5, orchestrator decision 2026-09-27 item 3): deadline, timeoutKind,
// a `phaseTimedOut`/`phaseInterrupted` phase, and a `jobKind` field. None has
// a production caller in phase 1 -- they belong to the phase that delivers
// them (timeouts: phase 2/5; host-lease interrupt: phase 4) rather than
// sitting unused ahead of that work.
package agent

import "context"

// jobPhase is a job's position in the ASYNC-03 state machine: exactly one
// terminal outcome, reached at most once, followed by at most one delivery.
// "Delivered" is deliberately not a jobPhase value (see deliverLocked): it is
// represented by the job's removal from sessionJobs.jobs, matching today's
// releaseLocked. A separate bool would duplicate that map state and could
// drift from it.
type jobPhase int32

const (
	phaseRunning jobPhase = iota
	phaseCompleted
	phaseFailed
	phaseCancelled
)

func (p jobPhase) terminal() bool { return p != phaseRunning }

// jobResult is a job's outcome, without owner/toolName -- both already live
// on the asyncJob itself and are not duplicated here. Assembled into an
// AsyncCompletion only at the delivery boundary (deliverLocked).
type jobResult struct {
	content string
	isError bool
}

// asyncJob is one work-ledger entry, keyed by (owner, toolCallID).
type asyncJob struct {
	owner        string // session the notice is delivered to
	toolCallID   string // key together with owner; stable, gives Start its idempotency
	input        string // tool call input; a reused id joins only for the same input
	toolName     string // "bash"/"run_command"/"agent"/"agentic_fetch" -- AsyncCompletion.ToolName
	childSession string // non-empty only for a delegation (agent/agentic_fetch); see Start
	cli          bool   // origin, verbatim today's asyncJobState.cli -- delivery routing unchanged (see deliverLocked)

	state     jobPhase
	announced bool               // ack-gate: the "started" tool result is persisted (onToolResult)
	cancel    context.CancelFunc // this job's executor context
	// result is valid once state.terminal(). For an armed-but-still-running
	// delegation (state still phaseRunning, indexed in workLedger.byChild),
	// it holds the result captured at the child's first turn -- see
	// workLedger.armDelegation's doc for why that is the end of a model
	// TURN, not the end of the child's work.
	result jobResult
}

// transitionToTerminal is the ONLY writer of state past phaseRunning. Called
// under workLedger.mu from finish/cancelSession/close/recheckChild. Returns
// false (no-op) once the job is already terminal -- that is the CAS:
// whichever caller holds workLedger.mu first for this job wins; every later
// caller for the SAME job is a no-op. Closes BL-2/#1032
// (docs/async-invariants.md, ASYNC-03): today's cancelSession swaps the
// whole owning session's job map out from under a concurrent finish
// (async_job_registry.go's old cancelSession/finish pair); here both sides
// change the SAME field of the SAME *asyncJob under the SAME mutex, so "the
// map was swapped out from under a concurrent finish" is structurally
// impossible.
//
// "CAS" here is not a lock-free atomic -- it is a mutex-protected
// check-then-set inside one critical section. That mutex already guards the
// whole ledger, so a second locking layer would be a new deadlock risk for
// no measured benefit.
func (j *asyncJob) transitionToTerminal(state jobPhase, result jobResult) bool {
	if j.state != phaseRunning {
		return false
	}
	j.state = state
	j.result = result
	return true
}
