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

import (
	"context"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// childScopeDBCheckTimeout bounds childScopeOpenAcrossProcesses's DB read
// (B3/C6 fix): recheckChild runs synchronously on several hot paths, so this
// stays well under a request-scoped budget rather than the 30s ceiling used
// for fire-and-forget background writes elsewhere in this package.
const childScopeDBCheckTimeout = 5 * time.Second

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
func (l *workLedger) armDelegation(job *asyncJob, captured jobResult) {
	if job == nil || job.childSession == "" {
		return
	}
	l.mu.Lock()
	if !l.currentLocked(job) {
		// Superseded (A11): a stale executor must not arm, or re-check on
		// behalf of, another claim's job.
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
// Phase-4 step 2: recheckChild does nothing once the ledger is closed (doc
// sec.3.1/3.7): a child turn interrupted by shutdown must not count as
// "child scope closed", so the delegation must not finish with truncated
// text. The release itself now goes through workLedger.transition
// (causeDelegationRelease) instead of calling transitionToTerminal
// directly, so it is DB-durable (DUR-1) like every other terminal cause.
func (l *workLedger) recheckChild(childSessionID string) {
	if childSessionID == "" {
		return
	}
	for {
		l.mu.Lock()
		closed := l.closed
		l.mu.Unlock()
		if closed {
			return
		}
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
		owner, toolCallID, sync := job.owner, job.toolCallID, job.sync
		snapshot := AsyncCompletion{
			SessionID: childSessionID, ToolCallID: toolCallID, ToolName: job.toolName,
			Content: job.result.content, IsError: job.result.isError,
		}
		l.mu.Unlock()

		refreshed := snapshot
		if l.coord != nil {
			refreshed = l.coord.refreshSubAgentCompletion(childSessionID, snapshot)
		}

		if sync {
			// Sync (SDK-origin) delegations never touch the store (doc
			// sec.3.1) -- old memory-only path, so awaitSync still unblocks
			// via deliverLocked's own sync branch (closes job.done).
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
		} else {
			l.transition(job, causeDelegationRelease, jobResult{content: refreshed.Content, isError: refreshed.IsError})
		}

		l.mu.Lock()
		stillRunning := job.state == phaseRunning
		l.mu.Unlock()
		if stillRunning {
			// l.transition SKIPPED -- a concurrent cause (e.g. cancelSession)
			// is already mid-transition for this SAME job. Don't spin on
			// it; a later trigger (its own completion, or another
			// recheckChild call) will re-check.
			return
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
	// B3/C6 fix: every check above is same-process/in-memory (this host's
	// own workLedger job map plus IsSessionBusy). A child whose own async
	// work moved to a DIFFERENT, still-live host, or whose pulled notice the
	// child still owes a reaction to (DB reaction debt), has NOT actually
	// drained -- releasing the delegation now would hand the parent a
	// premature/truncated result. childScopeOpenAcrossProcesses reuses
	// ScopeOpen's exact predicate (doc sec.3.5: running row on a live host,
	// OR reaction debt), deliberately omitting ScopeOpen's mid-turn branch
	// (already covered by the IsSessionBusy checks above).
	if l.childScopeOpenAcrossProcesses(childID) {
		return false
	}
	return true
}

// childScopeOpenAcrossProcesses is childScopeDrained's cross-process half
// (B3/C6 fix). Fails OPEN (i.e. reports true, "not drained yet") on a DB
// error or a nil store: prematurely releasing a delegation whose child
// might still owe a reaction is worse than deferring the release to a later
// recheckChild trigger -- the same asymmetry ScopeOpen's own callers rely
// on for scope evaluation.
func (l *workLedger) childScopeOpenAcrossProcesses(childID string) bool {
	if l.coord == nil || l.store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), childScopeDBCheckTimeout)
	defer cancel()
	open, err := l.coord.ScopeOpen(ctx, childID)
	if err != nil {
		slog.Warn("childScopeDrained: cross-process scope check failed; treating scope as still open", "child_session_id", childID, "err", err)
		return true
	}
	return open
}

// cancelSessionTarget is a snapshot of one job cancelSession must act on,
// captured entirely under l.mu so the rest of cancelSession's work (DB I/O,
// doc sec.3.1's "с DB I/O outside l.mu") never touches job fields without
// the lock.
type cancelSessionTarget struct {
	job          *asyncJob
	owner        string
	toolCallID   string
	toolName     string
	shellID      string
	outputBuf    tools.LiveOutputBuffer
	isDelegation bool
	sync         bool
	cancel       context.CancelFunc
}

// cancelSession forces every job sessionID owns, AND every delegation parked
// on it as a CHILD (byChild[sessionID]), to a terminal cancelled outcome.
// Replaces asyncJobRegistry.cancelSession + coordinator.
// releaseSubAgentOutcomesForParentCancel/ForChildCancel combined.
//
// Phase-4 step 2 (doc sec.3.4/3.8): every non-sync job now durably records
// cancelled/session_cancel/wake=0 via commitTransition/transition, snapshot
// -> transition -> cancel, with the DB I/O outside l.mu. A PLAIN job still
// produces NO in-memory notice (commitTransition directly, bypassing
// deliverLocked) -- only the DB row records the fact, same observable
// behavior as before. A DELEGATION job IS delivered with
// subAgentOutcomeCancelledText, exactly as before (via transition, which
// does call deliverLocked) -- a delegation canceled here never reads
// refreshSubAgentCompletion: the result is fixed to the cancellation text
// up front, so a child's already-finished successful turn can never
// masquerade as this delegation's outcome (regression:
// TestSubAgentOutcome_CancelSurvivesFinishedChildTurn). A SYNC job (plain or
// delegation) never touches the store -- it keeps the exact old in-memory-
// only path (transitionToTerminal+deliverLocked for a delegation, so
// awaitSync unblocks; a bare map delete for a plain job).
func (l *workLedger) cancelSession(sessionID string) {
	if sessionID == "" {
		return
	}
	l.mu.Lock()
	var targets []cancelSessionTarget
	if s := l.bySession[sessionID]; s != nil {
		for _, job := range s.jobs {
			// Review finding P2: mark BEFORE releasing l.mu, synchronously
			// with every other job this call targets -- this is what makes
			// the later race-safe regardless of which cause's DB write (this
			// call's, or a concurrent natural finish's) actually commits
			// first; see transition's delivery-step doc.
			job.stoppedBySession = true
			targets = append(targets, cancelSessionTarget{
				job: job, owner: job.owner, toolCallID: job.toolCallID, toolName: job.toolName,
				shellID: job.shellID, outputBuf: job.outputBuf, isDelegation: job.childSession != "",
				sync: job.sync, cancel: job.cancel,
			})
		}
		signalWorkSession(s)
	}
	for _, job := range l.byChild[sessionID] {
		job.stoppedBySession = true
		targets = append(targets, cancelSessionTarget{
			job: job, owner: job.owner, toolCallID: job.toolCallID, toolName: job.toolName,
			shellID: job.shellID, outputBuf: job.outputBuf, isDelegation: true,
			sync: job.sync, cancel: job.cancel,
		})
	}
	l.mu.Unlock()

	l.stopTargets(targets)
	// A cancelled session's own supervision timer (if any) is dropped here
	// unconditionally -- cancelling is this codebase's closest existing
	// proxy for "give up on this session's work" -- see
	// clearSupervisionIfPresent's own doc for the known gap around a bare
	// session delete with no prior cancel.
	l.clearSupervisionIfPresent(sessionID)
}

// stopTargets runs the actual stop for every snapshotted target (cancelSession's
// original inline loop, factored out so work_ledger_rerun.go's Rerun-scoped
// stop can share it exactly): sync delegations unblock awaitSync in memory,
// sync plain jobs are dropped from the map, and every non-sync job (plain or
// delegation) takes the one durable transition path. Caller must NOT hold
// l.mu; every target's stoppedBySession/killRequested marker (whichever the
// caller set) must already be recorded under l.mu before calling this, so
// the race described on cancelSession's own targets-capture loop is closed
// regardless of which caller populated targets.
func (l *workLedger) stopTargets(targets []cancelSessionTarget) {
	for _, tgt := range targets {
		switch {
		case tgt.sync && tgt.isDelegation:
			// Old memory-only path: unblocks awaitSync via deliverLocked's
			// own sync branch (closes job.done). Sync jobs never touch the
			// store (doc sec.3.1).
			l.mu.Lock()
			tgt.job.transitionToTerminal(phaseCancelled, jobResult{content: subAgentOutcomeCancelledText, isError: true})
			completion, callback := l.deliverLocked(tgt.owner, tgt.job)
			l.mu.Unlock()
			if callback {
				l.onWebDone(completion)
			}
		case tgt.sync:
			l.mu.Lock()
			if s := l.bySession[tgt.owner]; s != nil && s.jobs[tgt.toolCallID] == tgt.job {
				delete(s.jobs, tgt.toolCallID)
				signalWorkSession(s)
			}
			l.mu.Unlock()
		default:
			// Non-sync (plain OR delegation): ONE durable path (review
			// finding P2 -- "one path is preferred"). transition's own
			// delivery step drops a stoppedBySession-marked PLAIN job
			// silently no matter which cause wins the CAS race, and still
			// delivers a delegation's cancelled notice exactly as before.
			content := jobResult{content: subAgentOutcomeCancelledText, isError: true}
			if !tgt.isDelegation {
				content = l.capturePartial(tgt.owner, tgt.toolCallID, tgt.toolName, "", tgt.shellID, tgt.outputBuf)
			}
			l.transition(tgt.job, causeSessionCancel, content)
		}
		if tgt.cancel != nil {
			tgt.cancel()
		}
	}
}

// treeSessionIDs returns root plus every descendant reachable through a
// still-RUNNING delegation job (doc sec.3.8: "Stop is transitive: cancelling
// a session cancels the whole tree via running delegation rows"), walked and
// snapshotted BEFORE any cancellation runs so the walk sees the pre-Stop
// tree shape rather than racing its own cancellations. Descendants via
// parent_session_id are deliberately NOT walked (doc sec.3.5's same rule):
// only a delegation row still RUNNING represents live child scope.
func (l *workLedger) treeSessionIDs(root string) []string {
	if root == "" {
		return nil
	}
	seen := map[string]bool{root: true}
	order := []string{root}
	frontier := []string{root}
	for len(frontier) > 0 {
		var next []string
		l.mu.Lock()
		for _, id := range frontier {
			s := l.bySession[id]
			if s == nil {
				continue
			}
			for _, job := range s.jobs {
				if job.childSession != "" && job.state == phaseRunning && !seen[job.childSession] {
					seen[job.childSession] = true
					next = append(next, job.childSession)
				}
			}
		}
		l.mu.Unlock()
		order = append(order, next...)
		frontier = next
	}
	return order
}

// cancelTree cancels root and every descendant session in its current
// delegation tree (treeSessionIDs), returning every id it acted on so the
// caller (coordinator.Cancel) can also hard-stop each session's mailbox and
// zero the wake bit on every already-terminal debt row across the whole set
// (doc sec.3.4/3.8: a race between a natural completion and Stop must never
// grant a stopped delegation's child a turn).
func (l *workLedger) cancelTree(root string) []string {
	ids := l.treeSessionIDs(root)
	for _, id := range ids {
		l.cancelSession(id)
	}
	return ids
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

// parkedChildSessions returns the distinct CHILD session ids (byChild's own
// keys) that still have at least one armed delegation -- i.e. exactly the
// ids recheckChild itself expects (it indexes l.byChild by CHILD id, never
// by parent/owner). B12/C14 fix: RecheckPass's 60s pass used to call
// recheckChild with parkedParentSessions()' PARENT ids instead, so
// byChild[parent] was always a miss and no child was ever actually
// re-evaluated by the pass.
func (l *workLedger) parkedChildSessions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	children := make([]string, 0, len(l.byChild))
	for childID, entries := range l.byChild {
		for _, job := range entries {
			if job.state == phaseRunning {
				children = append(children, childID)
				break
			}
		}
	}
	return children
}
