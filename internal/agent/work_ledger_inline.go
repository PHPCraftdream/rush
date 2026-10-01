// The A14 inline window (docs/plans/2026-10-01-inline-window.md): a
// bash/run_command job whose natural terminal transition (Tx1) commits
// within the window is answered by its own tool call -- the inline response
// carries the result and closes the row's delivery in the fused Tx2
// (session.AsyncJobStore.AnnounceInlineResult) -- instead of today's
// "started" response plus a later pulled notice. Every delay here is a
// deferred "started", never a lost job: if anything but a natural
// completed/failed outcome happens inside the window (Stop, job_kill, a
// timeout, ctx cancellation, ledger close), the caller falls back to the
// ordinary startedResponse and today's delivery path untouched.
package agent

import (
	"context"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// inlineWindowSeconds is the fixed, non-configurable window (design doc
// "N and scope": a constant, not a setting). The compile-time assertion
// below keeps it strictly under the explicit-timeout floor: a timeout{} of
// timeoutSecondsFloor (5s) or more can then never fire inside a 3s window,
// so the inline decision never has to consider one.
const inlineWindowSeconds = 3

const inlineWindow = inlineWindowSeconds * time.Second

// Compile-time assert: inlineWindowSeconds+1 <= timeoutSecondsFloor must
// hold (a zero denominator fails the build), so a legal explicit timeout is
// always longer than the window.
const _ = timeoutSecondsFloor / (inlineWindowSeconds + 1)

// inlineWindowApplies reports whether toolName ever runs inside the window.
// Only bash and run_command: agent/agentic_fetch finalize through
// armDelegation and are NEVER inline (design doc "N and scope").
func inlineWindowApplies(toolName string) bool {
	return toolName == tools.BashToolName || toolName == tools.RunCommandToolName
}

// inlineWindowDisabler is the test seam the app package reaches through:
// only the real coordinator's ledger carries the window; test fakes that
// implement Coordinator another way simply return false.
type inlineWindowDisabler interface {
	disableInlineWindowForTest()
}

// SetInlineWindowForTest zeroes (on=false) or restores (on=true) the
// coordinator's inline window -- the A14 test seam: 0 = the pre-A14
// "started" flow, byte for byte. package app's e2e harnesses disable it
// for scenarios whose scripts match the literal "Async ... started" text or
// count pulled notices; the window-on e2e re-enables it explicitly.
// Returns false for a coordinator with no window to set (test fakes).
func SetInlineWindowForTest(c Coordinator, on bool) bool {
	if s, ok := c.(inlineWindowSetter); ok {
		s.setInlineWindowForTest(on)
		return true
	}
	return false
}

type inlineWindowSetter interface {
	setInlineWindowForTest(on bool)
}

func (c *coordinator) setInlineWindowForTest(on bool) {
	if c != nil && c.asyncJobs != nil {
		if on {
			c.asyncJobs.inlineWindow = inlineWindow
		} else {
			c.asyncJobs.inlineWindow = 0
		}
	}
}

// awaitInline blocks for at most the inline window, waiting for job's Tx1
// (transitionToTerminal after commitTransition's DB CAS). It returns the
// job's committed result and true only when the job may be answered inline:
// still the ledger's entry for its key, naturally terminal
// (completed/failed), unannounced, not being acknowledged, not stopped by
// the session, not job-killed. Every other outcome -- window elapsed,
// caller's ctx done, ledger closed, any other phase (cancelled/timed_out),
// or the job replaced under the same key -- returns false and the caller
// answers with the ordinary startedResponse. On true, the caller's right to
// acknowledge is claimed HERE (job.inlinePending/job.acking under l.mu), so
// exactly one tool result -- the inline one -- can ever settle this job.
func (l *workLedger) awaitInline(ctx context.Context, job *asyncJob) (jobResult, bool) {
	if l == nil || job == nil || job.settled == nil {
		return jobResult{}, false
	}
	timer := time.NewTimer(l.inlineWindow)
	defer timer.Stop()
	select {
	case <-job.settled:
	case <-timer.C:
		return jobResult{}, false
	case <-ctx.Done():
		return jobResult{}, false
	case <-l.closedCh:
		return jobResult{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.currentLocked(job) ||
		(job.state != phaseCompleted && job.state != phaseFailed) ||
		job.announced || job.acking || job.stoppedBySession || job.killRequested {
		// Stop-in-window orphan guard: a job Stop made terminal while it was
		// still awaiting its "started" ack is normally dropped by
		// deliverLocked the moment that ack flips announced -- but the ack
		// itself may never come (the turn was cancelled; the provider stream
		// ended without an OnToolResult). Dropping it here, on the loser of
		// the inline decision, keeps the dead call from holding a ledger
		// slot (the limit of 50) and l.running forever. The durable row
		// already records the cancellation (wake=0, no debt, no notice);
		// a late ack for the dropped job resolves through claimAck's nil
		// path (a plain Create).
		if job.stoppedBySession && job.state.terminal() && !job.announced {
			l.dropStoppedInlineLocked(job)
		}
		return jobResult{}, false
	}
	job.acking = true
	job.inlinePending = true
	return job.result, true
}

// dropStoppedInlineLocked removes a Stop-cancelled, unannounced windowed job
// from its owner's map (the same bookkeeping deliverLocked's stoppedBySession
// branch performs). Caller holds l.mu and currentLocked(job).
func (l *workLedger) dropStoppedInlineLocked(job *asyncJob) {
	s := l.bySession[job.owner]
	if s == nil {
		return
	}
	delete(s.jobs, job.toolCallID)
	if len(s.jobs) == 0 {
		l.clearSupervisionIfPresent(job.owner)
	}
	signalWorkSession(s)
}

// finishInlineLocally is the inline ack's in-memory tail: the same scope
// bookkeeping deliverLocked performs (announced flip, map drop, supervision
// clear, work signal) WITHOUT its onWebDone callback -- the inline response
// already carried the result to the model inside this turn, so there is no
// wake hint to submit and no child to re-check (bash/run_command have no
// child session). A job already removed (a racing delivery or abort won)
// is left alone.
func (l *workLedger) finishInlineLocally(job *asyncJob) {
	if l == nil || job == nil {
		return
	}
	l.mu.Lock()
	if !l.currentLocked(job) {
		l.mu.Unlock()
		return
	}
	job.announced = true
	s := l.bySession[job.owner]
	delete(s.jobs, job.toolCallID)
	if len(s.jobs) == 0 {
		l.clearSupervisionIfPresent(job.owner)
	}
	signalWorkSession(s)
	l.mu.Unlock()
}

// acknowledgeInlineResult is persistToolResult's inline branch (the
// ackTag.Inline tag claimAck handed over): the fused Tx2 via
// store.AnnounceInlineResult, then finishInlineLocally. handled=false
// means the caller falls back to the ordinary plain-Create path:
//   - ErrAsyncJobGone (the row was deleted out from under the job): the
//     whole transaction rolled back, so the result is persisted the ordinary
//     way (B15's shape) and acknowledged tolerates the gone row;
//   - a sync job or no store (cannot happen for an inline-tagged job today,
//     but claimAck's contract lets this branch answer defensively).
//
// A 0-rows delivery CAS is NOT a fallback (only a Rerun's void or a delete
// reaches it -- the caller verified the committed terminal state under
// l.mu moments earlier): the announce half committed, so the result stays
// in history, and the ordinary tail runs with no delivery left to close.
// Any other store error aborts the job like every other failed ack write.
func (l *workLedger) acknowledgeInlineResult(ctx context.Context, job *asyncJob, messages message.Service, params message.CreateMessageParams) (message.Message, bool, error) {
	l.mu.Lock()
	store := l.store
	l.mu.Unlock()
	if job.sync || store == nil {
		return message.Message{}, false, nil
	}
	ann, err := store.AnnounceInlineResult(ctx, messages, job.owner, job.toolCallID, job.claimID, params)
	if err != nil {
		if errors.Is(err, session.ErrAsyncJobGone) {
			return message.Message{}, false, nil
		}
		l.abort(job)
		return message.Message{}, true, err
	}
	l.finishInlineLocally(job)
	return ann.Msg, true, nil
}
