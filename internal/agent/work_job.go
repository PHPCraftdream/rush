// Single work-ledger job record (phase 1 of the async structured-concurrency
// rewrite, docs/plans/2026-09-27-async-structured-concurrency.md). Replaces
// asyncJobState (former async_job_registry.go) and subAgentOutcomeEntry
// (former subagent_outcome.go) with one type: a delegation (`agent`/
// `agentic_fetch`) used to have TWO records across two registries for one
// call's whole lifecycle; here there is one, from Start to delivery.
//
// Phase 2 (docs/plans/2026-09-27-async-phase2-spec.md §5.1) adds the explicit
// per-call timeout fields phase 1 deliberately left out (deadline,
// timeoutKind, timeoutSeconds, timeoutNotified, phaseTimedOut) and the
// sync/done pair for the unified asyncTool execution path (§4.4).
package agent

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

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
	phaseTimedOut // terminal: reached only via handleTimeout's timeoutTerminateAndWake branch (§5.5)
)

func (p jobPhase) terminal() bool { return p != phaseRunning }

// timeoutKind is the explicit, no-default kind chosen PER CALL (operator
// decision, docs/plans/2026-09-24-agent-wakes-and-async-job-control.md,
// "Решено 2026-09-27" item 4; wire values fixed by
// docs/plans/2026-09-27-wake-tools-contract.md §3). timeoutNone is the zero
// value: "no timeout" requires no special-casing at any call site.
type timeoutKind uint8

const (
	timeoutNone             timeoutKind = iota
	timeoutWakeOnly                     // "wake_only": deadline passed -> job keeps running; owner gets a NON-terminal "time's up, still running" event with a summary
	timeoutTerminateAndWake             // "terminate_and_wake": deadline passed -> job -> phaseTimedOut (terminal), partial output kept, owner gets the normal terminal notice
)

// TimeoutSpec is an optional, always-explicit per-call deadline. nil means
// "no ledger-level timeout" -- the existing 45-minute tool watchdog
// (agent.go's toolExecutionMaxDefault/toolMaxDuration) is a SEPARATE,
// unrelated mechanism and is not touched by this type. Seconds is kept
// alongside Deadline (redundant with it, Deadline == now+Seconds at
// construction) purely so the eventual notice text (contract §5.1: "timed
// out after {seconds}s") can quote the ORIGINALLY REQUESTED duration instead
// of a recomputed, possibly-off-by-scheduling-jitter value.
type TimeoutSpec struct {
	Deadline time.Time
	Kind     timeoutKind
	Seconds  int
}

// jobResult is a job's outcome, without owner/toolName -- both already live
// on the asyncJob itself and are not duplicated here. Assembled into an
// AsyncCompletion only at the delivery boundary (deliverLocked).
type jobResult struct {
	content string
	isError bool
	// metadata is the inner tool's raw ToolResponse.Metadata (JSON string),
	// preserved so a sync job's awaitAndFinish (§4.4) can reconstruct a
	// byte-for-byte equivalent response instead of a bare text one. Empty
	// (and unused) for every async delivery, which only ever formats
	// content/isError into a human notice.
	metadata string
}

// asyncJob is one work-ledger entry, keyed by (owner, toolCallID).
type asyncJob struct {
	owner        string // session the notice is delivered to
	toolCallID   string // key together with owner; stable, gives Start its idempotency
	input        string // tool call input; a reused id joins only for the same input
	toolName     string // "bash"/"run_command"/"agent"/"agentic_fetch" -- AsyncCompletion.ToolName
	childSession string // non-empty only for a delegation (agent/agentic_fetch); see Start
	cli          bool   // origin, verbatim today's asyncJobState.cli -- delivery routing unchanged (see deliverLocked)
	// claimID (A11) is the claim_id store.Claim minted for THIS job's row,
	// carried for the job's whole lifetime and threaded into every
	// commitTransition call's TransitionParams.ClaimID -- closes the ABA
	// where a deleted-then-re-claimed row lets a stale executor's late
	// result commit onto a DIFFERENT (fresh) claim of the same (owner,
	// toolCallID) key. Empty for a sync job (never touches the store).
	claimID string
	// startedAt is when Start registered this job. Used only by supervision's
	// summary (supervision.go) to report how long each open job has run --
	// no other reader needs it, so it is not threaded into AsyncCompletion.
	startedAt time.Time

	state     jobPhase
	announced bool // ack-gate: the "started" tool result is persisted (onToolResult)
	// acking is set by claimAck under l.mu for the one tool result that has
	// the right to acknowledge or abort this job (R2B-3), so a concurrent or
	// repeated tagged result cannot start a second announce while the first
	// is still writing.
	acking bool
	cancel context.CancelFunc // this job's executor context
	// result is valid once state.terminal(). For an armed-but-still-running
	// delegation (state still phaseRunning, indexed in workLedger.byChild),
	// it holds the result captured at the child's first turn -- see
	// workLedger.armDelegation's doc for why that is the end of a model
	// TURN, not the end of the child's work.
	result jobResult
	// shellID is the background shell id backing a bash job, set by
	// workLedger.setShellID once asyncTool.awaitShell learns it from the
	// inner tool's response metadata (empty until then, and always empty for
	// run_command/agent/agentic_fetch jobs). Lets job_kill/job_output resolve
	// the job id the model saw (toolCallID) to the shell id they need -- see
	// workLedger.ResolveJobShellID (task #1053).
	shellID string

	// sync is true for a job started for a non-CLI/non-Web origin (SDK,
	// unspecified): its only consumer is the goroutine blocked in awaitSync
	// (§4.4), so it is never routed to the ready queue or onWebDone.
	sync bool
	// done is non-nil only when sync; closed exactly once, by deliverLocked,
	// when this job's outcome is ready for awaitSync to read.
	done chan struct{}

	// deadline is this job's explicit timeout deadline (§5.1); zero means no
	// timeout. timeoutKind/timeoutSeconds are meaningless when deadline is
	// zero. timeoutNotified is a one-shot guard so a timeoutWakeOnly event
	// fires at most once per deadline, never repeats.
	deadline        time.Time
	timeoutKind     timeoutKind
	timeoutSeconds  int
	timeoutNotified bool

	// transitioning is the phase-4 step-2 "переход идёт" in-flight latch
	// (doc sec.3.1/DUR-1): set by workLedger.transition while its retry loop
	// is running store.Transition for THIS job, outside l.mu. A concurrent
	// second cause for the same job SKIPS instead of waiting -- see
	// workLedger.transition's doc.
	transitioning bool
	// shutdownCancelled is set by workLedger.close() right before it cancels
	// this job's executor context (doc sec.3.1/3.7's shutdown latch): "caused
	// by process shutdown" is workLedger.closed AND this flag, checked inside
	// transition's retry loop. A transition suppressed this way writes
	// nothing to the DB -- the row stays 'running' for the next host to
	// recover. close() only sets this for a job that is BOTH non-terminal
	// and not executorReturned (B9): a job whose real, natural outcome is
	// already known (or being committed) must never be mistaken for one
	// close() itself is cancelling, or its legitimate result is silently
	// discarded and the row is left 'running' for no reason.
	shutdownCancelled bool
	// executorReturned is set by finish() (the plain-job natural-completion
	// path) the INSTANT it acquires l.mu, before any DB work (B9): close()
	// consults this so it never latches shutdownCancelled onto a job whose
	// executor has ALREADY produced its real result and is merely waiting
	// its turn for l.mu to report it -- a job cancelled by close() strictly
	// BEFORE this flag exists to be checked. Not set by armDelegation's own
	// (separate file, out of this fix's scope) equivalent path.
	executorReturned bool
	// stoppedBySession is set by cancelSession, under l.mu, for every job it
	// targets, BEFORE releasing the lock to do its (now durable) DB I/O
	// (review finding P2). This closes a race a plain, non-delegation job
	// would otherwise lose: a concurrent natural finish() whose OWN
	// transition call happens to win the DB CAS ahead of cancelSession's
	// would otherwise still reach deliverLocked and wake the session right
	// after the user pressed Stop. Because the flag is set synchronously,
	// before either side's DB write even begins, transition's delivery step
	// can drop a marked PLAIN job silently regardless of which cause
	// actually won. Delegations ignore this flag -- they keep delivering
	// their cancelled notice exactly as before.
	stoppedBySession bool
	// killRequested is set (under workLedger.mu) the instant MarkJobStopped/
	// StopRunCommandJob begins acting on this job (task #1063), BEFORE the
	// snapshot/transition/kill sequence runs -- distinct from transitioning
	// (which only covers the DB-write window inside commitTransition): this
	// closes the true-concurrency window where a second job_kill call could
	// otherwise race ahead of the first's own transitioning flag and answer
	// as if it, too, were the fresh stop. A racer that observes this already
	// true gets JobStopNotFound from MarkJobStopped/an error from
	// StopRunCommandJob. For run_command this also means no second kill
	// attempt (StopRunCommandJob refuses before touching cancel()). For a
	// bash job_id, job_kill.go acts on the verdict (B11): NotFound and
	// AlreadyTerminal both return WITHOUT calling bgManager.KillOwned, only
	// JobStopStopped kills -- so a racer performs no kill of its own on
	// either path.
	killRequested bool
	// outputBuf is set by workLedger.setRunCommandBuffer once a run_command
	// job's live output sink registers (async_tool.go, task #1023 §3):
	// run_command has no BackgroundShellManager entry, so this is the only
	// way to read its output, or quote partial output in a stopped/timeout
	// notice, while it is still running. Always nil for bash/agent/
	// agentic_fetch jobs.
	outputBuf tools.LiveOutputBuffer

	// wake is the COMMITTED row's own wake bit (phase-4 step 3, doc
	// sec.3.4's wake-policy table), set by commitTransition from
	// outcome.Row.Wake right after transitionToTerminal. Meaningless (zero
	// value) until state.terminal(); a sync job never sets it (no DB row) --
	// see AsyncCompletion.Wake's doc for why callers only ever read this for
	// a non-sync completion.
	wake bool
}

// transitionToTerminal is the in-memory half of a terminal transition,
// called under workLedger.mu ONLY from workLedger.transition (work_ledger_
// transition.go) -- the single async-job writer (DUR-1) that first commits
// the DB CAS, then adopts its committed row into memory here. Returns false
// (no-op) once the job is already terminal -- that is the in-memory CAS:
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
