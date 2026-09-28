// Delegation half of workLedger: parking a delegated sub-agent's (`agent`/
// `agentic_fetch`) completion until the child session's own async work
// drains, plus cancellation (both directions: owner and delegated-child).
// Absorbs subagent_outcome.go's byChild index into the same job records the
// plain half (work_ledger.go) already tracks, so the "two records for one
// call" class of bug (finishParked's re-insert branch, BL-2/#1032) has no
// seam left to hide in.
//
// The `agent` and `agentic_fetch` tools are dispatched asynchronously for CLI
// and web origins. The completion delivered to the PARENT session used to be
// producible the instant the CHILD's Run() returned, which is only the end of
// one model TURN: a child that started its own async tools or background
// shells has yielded, not finished. armDelegation captures that turn's result
// without transitioning the job to terminal; recheckChild transitions and
// delivers it once the child's OWN work (childScopeDrained) is terminal too.
//
// Phase 3 (docs/plans/2026-09-28-async-phase3-spec.md §4) deletes the
// safety-net ticker this file used to run (subAgentOutcomeTickInterval,
// startTickerLocked, tick): its own re-check triggers -- (i) the arm site
// itself, (ii) a child async-job completion, (iii) a child background-job
// completion, and (iv) the end of a child run, now including
// sessionAgent.onSessionIdle's by-construction release (agent_ownership.go)
// -- are exhaustive for today's depth-1 delegation tree (see the spec's §1.4
// trace and the phase-3 step-0 tests), and the ticker's own start/stop had a
// real race (BL-2026-09-25-1, closed here by deletion rather than a fix).
package agent

// subAgentOutcomeCancelledText is the body delivered to the parent when a
// delegation is released by Cancel/CancelAll rather than by the child
// finishing its work.
const subAgentOutcomeCancelledText = "sub-agent canceled"

// armDelegation captures the result of a delegation's FIRST turn (the child's
// Run() returning) without transitioning the job to terminal, and indexes it
// under its child session for FIFO release. Replaces
// subAgentOutcomeRegistry.park -- there is no second record to create: the
// job already exists (from Start), so arming only touches its result and the
// byChild index.
//
// Synchronously tries the immediate release path (trigger (i), formerly a
// separate tryRelease call after park): recheckChild is called once armed,
// after releasing l.mu.
func (l *workLedger) armDelegation(owner, toolCallID string, captured jobResult) {
	l.mu.Lock()
	s := l.bySession[owner]
	if s == nil {
		l.mu.Unlock()
		return
	}
	job := s.jobs[toolCallID]
	if job == nil || job.childSession == "" {
		l.mu.Unlock()
		return
	}
	job.result = captured
	l.byChild[job.childSession] = append(l.byChild[job.childSession], job)
	childSession := job.childSession
	l.mu.Unlock()

	l.recheckChild(childSession)
}

// recheckChild re-evaluates every armed (state still phaseRunning), not yet
// terminal delegation parked for childSessionID, in FIFO order, and delivers
// each one whose child scope is drained (childScopeDrained). This is the
// single point for re-check triggers (ii)-(iv); trigger (i) lives in
// armDelegation above. Replaces subAgentOutcomeRegistry.tryRelease/
// nextReleasable/release/claim/emit.
//
// The refresh (coordinator.refreshSubAgentCompletion, a DB read) runs OUTSIDE
// l.mu on a snapshot copy, exactly like the registry it replaces: only the
// snapshot's local variables are touched during that window, never the job's
// own fields, so a concurrent cancelSession/finish for the SAME job (racing
// under the SAME mutex) can freely win in between -- transitionToTerminal's
// CAS then simply loses here and this iteration's refreshed result is
// discarded. That is what makes recheckChild race-safe against cancelSession
// without a separate "claimed" latch: the mutex-protected state field IS the
// latch (see asyncJob.transitionToTerminal's doc).
func (l *workLedger) recheckChild(childSessionID string) {
	if childSessionID == "" {
		return
	}
	for {
		if !l.childScopeDrained(childSessionID) {
			return
		}
		l.mu.Lock()
		job := oldestArmedLocked(l.byChild[childSessionID])
		if job == nil {
			l.mu.Unlock()
			// §6.2: nothing left armed for childSessionID -- its scope is
			// confirmed closed (childScopeDrained already returned true
			// above). Release its driver/allowlist entry now, at the one
			// point that already knows this precisely; a no-op for a
			// childSessionID that was never a delegated child (get()
			// returns ok=false) or was never armed to begin with.
			if l.coord != nil {
				l.coord.releaseDriverIfScopeClosed(childSessionID)
			}
			return
		}
		owner, toolCallID := job.owner, job.toolCallID
		snapshot := AsyncCompletion{
			SessionID: childSessionID, ToolCallID: toolCallID, ToolName: job.toolName,
			Content: job.result.content, IsError: job.result.isError,
		}
		l.mu.Unlock()

		refreshed := snapshot
		if l.coord != nil {
			refreshed = l.coord.refreshSubAgentCompletion(childSessionID, snapshot)
		}

		l.mu.Lock()
		state := phaseCompleted
		if refreshed.IsError {
			state = phaseFailed
		}
		job.transitionToTerminal(state, jobResult{content: refreshed.Content, isError: refreshed.IsError})
		completion, callback := l.deliverLocked(owner, job)
		l.mu.Unlock()
		if callback {
			l.onWebDone(completion)
		}
	}
}

// oldestArmedLocked returns the oldest still-running (armed, not yet
// terminal) job in entries, or nil. Caller must hold l.mu.
func oldestArmedLocked(entries []*asyncJob) *asyncJob {
	for _, job := range entries {
		if job.state == phaseRunning {
			return job
		}
	}
	return nil
}

// childScopeDrained reports whether childID's OWN asynchronous work has
// reached a terminal state, i.e. whether a delegation parked on it may be
// released. Replaces coordinator.subAgentWorkTerminal.
//
// The busy check is a NEGATIVE gate only: it closes the window where an
// auto-resume turn has started but has not yet registered its next async
// job, so a release fired in that window would be premature. It queries
// whichever SessionAgent actually drives childID's turns -- its registered
// driver (runSubAgent registers one before starting a delegated child,
// coordinator_subagent_drivers.go), falling back to l.coord.currentAgent for
// a childID with no driver (bare test fixtures; a session that was never a
// delegation target). Querying currentAgent unconditionally would ask the
// WRONG SessionAgent for any real delegated child (task #1049).
//
// l.coord == nil (an isolated ledger test with no coordinator wired) is
// treated as "nothing more to check": the async-job/background/busy checks
// below all need a coordinator, so there is nothing left to gate on once the
// ledger itself reports no running job for childID.
func (l *workLedger) childScopeDrained(childID string) bool {
	if childID == "" {
		return false
	}
	if l.running(childID) {
		return false
	}
	if l.coord == nil {
		return true
	}
	if l.coord.background != nil && l.coord.background.ActiveOwned(childID) > 0 {
		return false
	}
	if driver, ok := l.coord.subAgentDrivers.get(childID); ok {
		if driver.agent.IsSessionBusy(childID) {
			return false
		}
	} else if l.coord.currentAgent != nil && l.coord.currentAgent.IsSessionBusy(childID) {
		return false
	}
	return true
}

// cancelSession forces every job sessionID owns, AND every delegation parked
// on it as a CHILD (byChild[sessionID]), to a terminal cancelled outcome, in
// one pass under one mutex hold. Replaces asyncJobRegistry.cancelSession +
// coordinator.releaseSubAgentOutcomesForParentCancel/ForChildCancel combined.
//
// A PLAIN job (childSession == "") is dropped without going through
// deliverLocked at all -- exactly like today's asyncJobRegistry.cancelSession,
// which never produces a notice for a canceled bash/run_command job. A
// DELEGATION job (childSession != "") IS delivered, with
// subAgentOutcomeCancelledText, exactly like today's
// coordinator.releaseCanceled -- deliverLocked is what a canceled delegation
// has always gone through (via finishParked), so this keeps producing the
// same one notice.
//
// A delegation canceled here never reads refreshSubAgentCompletion: the
// result is fixed to the cancellation text before deliverLocked runs, so a
// child's already-finished successful turn can never masquerade as this
// delegation's outcome (regression: TestSubAgentOutcome_CancelSurvivesFinishedChildTurn).
func (l *workLedger) cancelSession(sessionID string) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	var pending []AsyncCompletion
	if s := l.bySession[sessionID]; s != nil {
		for id, job := range s.jobs {
			if job.cancel != nil {
				job.cancel()
			}
			if job.childSession == "" {
				delete(s.jobs, id)
				continue
			}
			job.transitionToTerminal(phaseCancelled, jobResult{content: subAgentOutcomeCancelledText, isError: true})
			if completion, callback := l.deliverLocked(job.owner, job); callback {
				pending = append(pending, completion)
			}
		}
		signalWorkSession(s)
	}
	for _, job := range l.byChild[sessionID] {
		if job.cancel != nil {
			job.cancel()
		}
		job.transitionToTerminal(phaseCancelled, jobResult{content: subAgentOutcomeCancelledText, isError: true})
		if completion, callback := l.deliverLocked(job.owner, job); callback {
			pending = append(pending, completion)
		}
	}
	l.mu.Unlock()
	for _, completion := range pending {
		l.onWebDone(completion)
	}
}

// gcChildLocked drops fully-resolved (terminal) entries from childID's
// byChild bucket so the map cannot grow without bound over a long-lived
// coordinator. Caller must hold l.mu.
func (l *workLedger) gcChildLocked(childID string) {
	entries := l.byChild[childID]
	kept := entries[:0]
	for _, job := range entries {
		if job.state == phaseRunning {
			kept = append(kept, job)
		}
	}
	if len(kept) == 0 {
		delete(l.byChild, childID)
		return
	}
	l.byChild[childID] = kept
}

// hasParked reports whether any delegation is still armed (not yet
// terminal). Test-only observability now that the safety-net ticker
// (phase 3's former consumer of this) is gone.
func (l *workLedger) hasParked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entries := range l.byChild {
		for _, job := range entries {
			if job.state == phaseRunning {
				return true
			}
		}
	}
	return false
}

// parkedParentSessions returns the distinct owner session ids that still
// have at least one armed delegation. Used by the session status classifier
// so an at-rest parent that is still mid-workflow is not reported as done.
func (l *workLedger) parkedParentSessions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := make(map[string]struct{})
	parents := make([]string, 0, len(l.byChild))
	for _, entries := range l.byChild {
		for _, job := range entries {
			if job.state != phaseRunning {
				continue
			}
			if _, dup := seen[job.owner]; dup {
				continue
			}
			seen[job.owner] = struct{}{}
			parents = append(parents, job.owner)
		}
	}
	return parents
}
