// Rerun's async-job stop (doc sec.3.8, step 6): the ledger-level half of
// truncating a session's history out from under its own running tool calls.
// The DB-level void/repend reconciliation lives in
// internal/session/async_job_rerun.go; this file only stops the RUNNING
// rows the deleted tail names, recursively through a delegation's tree, the
// same way Stop does.
package agent

// stopToolCallsForRerun stops every RUNNING job owned by owner whose
// tool_call_id is in toolCallIDs, with job_kill semantics (wake=0, doc
// sec.3.8's Rerun paragraph): a plain job is stopped in place; a delegation
// is additionally cancelled through its WHOLE current tree (cancelTree),
// exactly like Stop, since its child session's own work must not outlive
// the deleted tool call that started it. Returns every child session id
// cancelTree acted on, across every delegation among toolCallIDs, so the
// caller can also fold them into the DB wake-zero pass Stop already does
// for its own tree (doc sec.3.4/3.8).
//
// Best-effort like cancelSession: a toolCallID with no RUNNING ledger entry
// (never started, already terminal, or owned by a dead/other host) is
// simply skipped here -- internal/session's void-by-tool-call-id step
// still protects its eventual DB row regardless of whether this function
// found anything to stop for it.
func (l *workLedger) stopToolCallsForRerun(owner string, toolCallIDs []string) (affectedChildSessions []string) {
	if owner == "" || len(toolCallIDs) == 0 {
		return nil
	}
	l.mu.Lock()
	s := l.bySession[owner]
	var targets []cancelSessionTarget
	var childSessions []string
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
			isDeleg := job.childSession != ""
			targets = append(targets, cancelSessionTarget{
				job: job, owner: job.owner, toolCallID: job.toolCallID, toolName: job.toolName,
				shellID: job.shellID, outputBuf: job.outputBuf, isDelegation: isDeleg,
				sync: job.sync, cancel: job.cancel,
			})
			if isDeleg {
				childSessions = append(childSessions, job.childSession)
			}
		}
	}
	l.mu.Unlock()

	l.stopTargets(targets)

	for _, child := range childSessions {
		affectedChildSessions = append(affectedChildSessions, l.cancelTree(child)...)
	}
	return affectedChildSessions
}
