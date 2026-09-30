// The phase-4 durable-core single writer (DUR-1, docs/plans/2026-09-28-
// async-phase4-durable-core.md sec.3.1/sec.5 step 2): transition is the ONLY
// function that ever moves a non-sync async job past phaseRunning. Every
// existing terminal-transition call site (finish, handleTimeout, job_kill/
// StopRunCommandJob, cancelSession, recheckChild) funnels through here
// instead of calling asyncJob.transitionToTerminal directly. The DB row
// commits first; memory adopts whatever committed (won or lost).
package agent

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
)

// transitionCause identifies WHY a terminal transition is happening, so
// transition can compute the DB state/notice_kind/wake once, in one place,
// instead of every caller duplicating doc sec.5 step 2's mapping table.
type transitionCause int

const (
	causeNaturalFinish transitionCause = iota
	causeTimeoutTerminated
	causeSessionCancel
	causeJobKill
	causeDelegationRelease
)

// transitionBackoffInitial/Cap are the growing-backoff bounds for the
// store-write retry loops (doc sec.3.1: "нарастающей паузой ... 10ms
// doubling to a 1s cap").
const (
	transitionBackoffInitial = 10 * time.Millisecond
	transitionBackoffCap     = 1 * time.Second
)

// asyncStoreRetryBackoff computes the doubling backoff for attempt n
// (0-based): 10ms, 20ms, 40ms, ... capped at 1s.
func asyncStoreRetryBackoff(attempt int) time.Duration {
	d := transitionBackoffInitial
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= transitionBackoffCap {
			return transitionBackoffCap
		}
	}
	return d
}

// retryAsyncStoreOp retries op with growing backoff until it succeeds, ctx is
// done, or the ledger is closed. Shared by the ack gate (acknowledged/
// store.MarkAnnounced) and abort (store.DeleteUnannounced) -- doc sec.5 step
// 2: "with the same retry rule" as transition, minus transition's own
// shutdown-latch check (neither of those two is tied to an executor
// cancelled by close()).
//
// P1 fix: acknowledged runs on the turn's tool-result path -- a DB that
// fails permanently must not hang that turn forever, and close() must still
// be able to release it (it used to keep retrying with ctx=
// context.Background(), which close() could never interrupt). The wait is
// now interruptible by l.closedCh, exactly like commitTransition's.
func (l *workLedger) retryAsyncStoreOp(ctx context.Context, op func() error) error {
	for attempt := 0; ; attempt++ {
		err := op()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-l.closedCh:
			return err
		case <-time.After(asyncStoreRetryBackoff(attempt)):
		}
	}
}

// causeStateNoticeKindWake maps a transitionCause to the DB state/
// notice_kind/wake/delivery/reacted quintuple (doc sec.5 step 2's table,
// extended by step 6's doc sec.3.2/3.4). delivery is "pending" -- a pull
// candidate -- for every cause except job_kill: its result is the job_kill
// TOOL's own answer (the real output snapshot, doc sec.3.2), so the row
// goes straight to delivery='done'/reacted=1 and never also surfaces as a
// second, pulled history notice. wake is already false for job_kill, which
// alone excludes it from the reaction-debt predicate; reacted=1 is
// belt-and-suspenders for any reader that checks delivery/reacted without
// wake.
func causeStateNoticeKindWake(cause transitionCause, result jobResult) (state, noticeKind, delivery string, wake, reacted bool) {
	delivery = "pending"
	switch cause {
	case causeNaturalFinish, causeDelegationRelease:
		wake = true
		if result.isError {
			state = "failed"
		} else {
			state = "completed"
		}
	case causeTimeoutTerminated:
		state, noticeKind, wake = "timed_out", "timeout_terminated", true
	case causeSessionCancel:
		state, noticeKind, wake = "cancelled", "session_cancel", false
	case causeJobKill:
		state, noticeKind, wake = "cancelled", "job_kill", false
		delivery, reacted = "done", true
	}
	return state, noticeKind, delivery, wake, reacted
}

// phaseForState maps a committed DB state string to its in-memory jobPhase.
// "interrupted" (written by dead-host recovery, DUR-6) has no dedicated
// phase; it degrades to phaseFailed so a row in that state is still treated
// as terminal-with-error rather than silently mishandled.
func phaseForState(state string) jobPhase {
	switch state {
	case "completed":
		return phaseCompleted
	case "cancelled":
		return phaseCancelled
	case "timed_out":
		return phaseTimedOut
	default: // "failed", "interrupted", or anything unrecognized
		return phaseFailed
	}
}

// asyncJobKindFor maps a tool name to the async_jobs.kind vocabulary (doc
// sec.5 step 2).
func asyncJobKindFor(toolName string) session.JobKind {
	switch toolName {
	case AgentToolName:
		return session.JobKindAgent
	case tools.AgenticFetchToolName:
		return session.JobKindFetch
	default: // BashToolName, RunCommandToolName
		return session.JobKindCommand
	}
}

// timeoutDeadlinePtr/timeoutKindString adapt a *TimeoutSpec (nil-able) to
// the store's Claim params.
func timeoutDeadlinePtr(t *TimeoutSpec) *time.Time {
	if t == nil || t.Deadline.IsZero() {
		return nil
	}
	d := t.Deadline
	return &d
}

func timeoutKindString(t *TimeoutSpec) string {
	if t == nil {
		return ""
	}
	switch t.Kind {
	case timeoutWakeOnly:
		return "wake_only"
	case timeoutTerminateAndWake:
		return "terminate_and_wake"
	default:
		return ""
	}
}

// commitOutcome is commitTransition's result -- what the durable store-write
// half of a transition actually did, WITHOUT delivering anything.
type commitOutcome int

const (
	commitSkipped commitOutcome = iota // already terminal, in-flight elsewhere, sync, no store, or shutdown-suppressed
	commitWon
	commitLost
	commitGone
)

// commitTransition is the DB-write + in-memory-adoption half of a terminal
// transition (DUR-1), WITHOUT delivery: transition (below) adds delivery
// for the normal case; cancelSession's plain-job path calls this directly
// and suppresses delivery entirely (doc sec.3.4: "a plain job cancelled by
// Stop produces no in-memory notice, the DB row records the fact").
//
// Caller must NOT hold l.mu. Blocks (with growing backoff) until the store
// commits, the row is found terminal/gone, the ledger is closed AND this
// job's executor was cancelled by close() itself (the shutdown latch, doc
// sec.3.1/3.7 -- nothing is written, row stays 'running'), or a RETRY wait
// is interrupted by the ledger closing for any other reason (review finding
// P1): a job whose own shutdownCancelled flag was never set (e.g. a natural
// finish already past its own executor's completion when close() ran) must
// still stop retrying once the ledger is closed, rather than leak a
// goroutine sleeping out backoff cycles against a DB the process may
// already be tearing down. Either way commitSkipped leaves the row exactly
// as the last successful commit (or lack thereof) left it -- a FIRST
// attempt that succeeds is always written, even if it races close().
//
// job is the caller's own *asyncJob, not a key (A11's in-process half): a
// job that is no longer the ledger's entry for its key -- dropped by an
// ack-abort, possibly replaced by a fresh claim of the same id -- is skipped
// here, so a stale executor can never commit onto its successor.
func (l *workLedger) commitTransition(job *asyncJob, cause transitionCause, result jobResult) commitOutcome {
	if job == nil {
		return commitSkipped
	}
	owner, toolCallID := job.owner, job.toolCallID
	l.mu.Lock()
	s := l.bySession[owner]
	if s == nil || s.jobs[toolCallID] != job || job.sync || job.state.terminal() || job.transitioning {
		// Already terminal, a sync job (never DB-backed), or another
		// goroutine's transition is already in flight for this SAME job
		// (doc sec.3.1's "защёлка «переход идёт» ... пропускают, не ждут"):
		// skip, never wait.
		l.mu.Unlock()
		return commitSkipped
	}
	job.transitioning = true
	store := l.store
	l.mu.Unlock()

	if store == nil {
		// No store wired -- production always wires one (Start fails
		// closed otherwise, DUR-8); an isolated test that never wires a
		// store must not reach here for a non-sync job either.
		l.mu.Lock()
		job.transitioning = false
		l.mu.Unlock()
		return commitSkipped
	}

	state, noticeKind, delivery, wake, reacted := causeStateNoticeKindWake(cause, result)
	var outcome session.TransitionResult
	for attempt := 0; ; attempt++ {
		l.mu.Lock()
		shuttingDown := l.closed && job.shutdownCancelled
		l.mu.Unlock()
		if shuttingDown {
			l.mu.Lock()
			job.transitioning = false
			l.mu.Unlock()
			return commitSkipped
		}
		var err error
		outcome, err = store.Transition(context.Background(), session.TransitionParams{
			Owner: owner, ToolCallID: toolCallID, State: state, NoticeKind: noticeKind,
			ResultSummary: result.content, ResultIsError: result.isError, Wake: wake,
			Delivery: delivery, Reacted: reacted,
			// ClaimID (A11): the claim this job's OWN Start captured -- closes
			// the ABA where a deleted-then-re-claimed row would otherwise let
			// this (now stale) executor's late result commit onto a fresh
			// claim of the same (owner, toolCallID) key just because both
			// have state='running'.
			ClaimID: job.claimID,
		})
		if err == nil {
			break
		}
		// P1 fix: the backoff wait is interruptible by the ledger closing --
		// select, not a plain time.Sleep -- so a retry loop returns promptly
		// once close() runs, instead of sleeping out the full remaining
		// backoff schedule against a DB the process may be tearing down.
		select {
		case <-l.closedCh:
			l.mu.Lock()
			job.transitioning = false
			l.mu.Unlock()
			return commitSkipped
		case <-time.After(asyncStoreRetryBackoff(attempt)):
		}
	}

	l.mu.Lock()
	job.transitioning = false
	if outcome.Outcome == session.TransitionLost && outcome.Row.State == "running" {
		// A11: the CAS lost NOT because some other cause already finished
		// this row (the pre-claim_id assumption every other TransitionLost
		// case still relies on below), but because claim_id no longer
		// matches -- the row was deleted and re-claimed under a DIFFERENT
		// claim while this job's OWN executor was still running (ABA). This
		// key is no longer ours to report ANY outcome for: treating
		// outcome.Row (still 'running', empty result) as if it were our
		// terminal state would mislabel this job phaseFailed with empty
		// content and deliver a false "failed with no output" completion to
		// the owner for a job that is actually still legitimately running
		// under the fresh claim. Drop from OUR map exactly like a gone row;
		// the fresh claim's own executor owns whatever happens to it next.
		l.dropLocked(job)
		l.mu.Unlock()
		return commitGone
	}
	switch outcome.Outcome {
	case session.TransitionGone:
		l.dropLocked(job)
		l.mu.Unlock()
		return commitGone
	case session.TransitionWon, session.TransitionLost:
		job.transitionToTerminal(phaseForState(outcome.Row.State), jobResult{
			content: outcome.Row.ResultSummary.String,
			isError: outcome.Row.ResultIsError.Int64 != 0,
		})
		// The COMMITTED row's own wake bit, not the cause's request: a
		// losing caller must adopt whatever the winner actually wrote (step
		// 3: AsyncCompletion.Wake below drives notifyAsyncCompletion's hint,
		// which must reflect the committed outcome, never the loser's own
		// (possibly different) cause).
		job.wake = outcome.Row.Wake != 0
	}
	l.mu.Unlock()
	if outcome.Outcome == session.TransitionWon {
		return commitWon
	}
	return commitLost
}

// jobOutcomeSnapshot is a post-commit view of a job returned by commitAndDeliver
// for a caller that must ANSWER based on what actually got committed (B11:
// job_kill), not assume its own cause won the race. found is false when the
// job could not be located at all afterward -- already fully delivered and
// removed from the map, or the row was gone (commitGone) -- meaning there is
// no committed row left to describe; the caller falls back to its own
// "not found" wording.
type jobOutcomeSnapshot struct {
	found  bool
	state  jobPhase
	result jobResult
}

// commitAndDeliver runs commitTransition then, for a won/lost outcome,
// performs the delivery step every terminal-transition call site needs:
// deliverLocked (which itself now owns the "a plain job stopped by Stop
// produces no in-memory notice" rule, doc sec.3.4 -- see that function's own
// doc for B10's fix). Returns the commit outcome plus a snapshot of the
// job's post-commit state/result so a caller that needs the COMMITTED
// outcome to build its own answer (B11) doesn't have to duplicate this
// delivery logic or assume its own cause was the one that actually won.
func (l *workLedger) commitAndDeliver(job *asyncJob, cause transitionCause, result jobResult) (commitOutcome, jobOutcomeSnapshot) {
	outcome := l.commitTransition(job, cause, result)
	var snap jobOutcomeSnapshot
	switch outcome {
	case commitWon, commitLost:
		l.mu.Lock()
		if !l.currentLocked(job) {
			l.mu.Unlock()
			return outcome, snap
		}
		snap = jobOutcomeSnapshot{found: true, state: job.state, result: job.result}
		completion, callback := l.deliverLocked(job.owner, job)
		l.mu.Unlock()
		if callback {
			l.onWebDone(completion)
		}
	}
	return outcome, snap
}

// transition is the single writer of a non-sync async job's terminal state,
// used by every non-sync terminal-transition call site (finish,
// handleTimeout, cancelSession, recheckChild) that does not need
// commitAndDeliver's return value. job_kill/StopRunCommandJob (work_ledger.go)
// call commitAndDeliver directly instead, since B11 requires their tool
// answer to reflect the COMMITTED outcome rather than assume causeJobKill
// won.
func (l *workLedger) transition(job *asyncJob, cause transitionCause, result jobResult) {
	l.commitAndDeliver(job, cause, result)
}

// transitionSync is the sync-job counterpart of every external cause (job_kill,
// terminate_and_wake timeout; review finding P2, regression of #1023 for
// library/SDK mode): a sync job never touches the store (doc sec.3.1), so
// its callers use this instead of transition to reach a well-formed terminal
// outcome entirely in memory -- transitionToTerminal + deliverLocked, the
// same pair cancelSession's own sync-delegation branch uses -- so a caller
// blocked in awaitSync gets that outcome (stopped/timed-out, with partial
// output) instead of the executor's own "context canceled" once the job's
// context is cancelled.
func (l *workLedger) transitionSync(job *asyncJob, phase jobPhase, result jobResult) {
	l.mu.Lock()
	if !l.currentLocked(job) || job.state.terminal() {
		l.mu.Unlock()
		return
	}
	job.transitionToTerminal(phase, result)
	completion, callback := l.deliverLocked(job.owner, job)
	l.mu.Unlock()
	if callback {
		l.onWebDone(completion)
	}
}
