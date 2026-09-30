// Rerun's async-job stop (doc sec.3.8, step 6): the ledger-level half of
// truncating a session's history out from under its own running tool calls.
// The durable void/re-pend reconciliation is session.TruncateForRerun's
// single transaction; this file only stops the RUNNING rows that
// transaction voided, and only after it committed.
package agent

// stopToolCallsForRerun stops every RUNNING job owned by owner whose
// tool_call_id is in toolCallIDs, with Stop semantics -- the same cause and
// path as cancelSession, NOT job_kill's: the job is marked stoppedBySession
// before l.mu is released and stopped in place through the one durable
// transition path as cancelled / notice_kind session_cancel / wake=0. Rerun
// has already voided these rows and a terminal transition preserves void, so
// a stopped row ends state=cancelled, delivery=void: `sessions jobs` shows a
// stopped job whose result belongs to the deleted branch, and (as for Stop)
// a plain job's in-memory notice is dropped. A delegation's child tree is NOT walked here:
// the caller (Coordinator.StopRerunJobs) stops it with stopTree, which
// also cancels the child's live generation and zeroes wake -- cancelTree
// alone did neither.
//
// Best effort: a toolCallID with no RUNNING ledger entry (never started,
// already terminal, or executed by another process) is skipped -- its row is
// already void, so its eventual terminal transition keeps void.
func (l *workLedger) stopToolCallsForRerun(owner string, toolCallIDs []string) {
	if owner == "" || len(toolCallIDs) == 0 {
		return
	}
	l.mu.Lock()
	s := l.bySession[owner]
	var targets []cancelSessionTarget
	if s != nil {
		for _, id := range toolCallIDs {
			job := s.jobs[id]
			if job == nil || job.state.terminal() {
				continue
			}
			// Same race closed by cancelSession's own doc: set BEFORE
			// releasing l.mu, synchronously with every other target this
			// call acts on, so a concurrent natural finish that wins the
			// CAS race cannot still hand a plain job's completion a wake.
			job.stoppedBySession = true
			targets = append(targets, cancelSessionTarget{
				job: job, owner: job.owner, toolCallID: job.toolCallID, toolName: job.toolName,
				shellID: job.shellID, outputBuf: job.outputBuf, isDelegation: job.childSession != "",
				sync: job.sync, cancel: job.cancel,
			})
		}
	}
	l.mu.Unlock()

	l.stopTargets(targets)
}
