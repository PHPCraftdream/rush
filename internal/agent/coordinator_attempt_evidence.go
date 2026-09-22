// Per-attempt evidence capture for coordinator_run.go's retry loop —
// split out because coordinator_run.go sits just under the repo's
// 1000-line limit (CLAUDE.md). Pure addition, no behavior moved.

package agent

import "sync"

// attemptEvidence is ONE run() attempt's capture target for the
// SessionAgentCall.OnAssistantMessageCreated callback (R3-1, round 6).
//
// coordinator_run.go used to write every attempt's callback into ONE
// closure variable shared by the whole runInternal call. Two defects
// followed. (A) Stale evidence: when Run queues the call (session busy)
// it returns (nil, nil) WITHOUT firing the callback, so the variable
// still held the PREVIOUS attempt's row ID and the retry classifiers
// judged the queued attempt on evidence it never produced -- requeueing
// a duplicate continuation behind the already-queued one. (B) Data
// race: mailbox.submit copies the call struct by value, so the queued
// copy carries the same closure; when a dispatcher goroutine later
// drains it, PrepareStep writes that shared variable with no
// synchronization against the coordinator goroutine's classifier
// reads.
//
// A fresh instance per attempt closes both by construction: a queued
// attempt's instance stays empty, so the classifiers see "" (no owned
// evidence) and refuse; a late callback from an async dispatcher
// writes into an already-abandoned instance whose state is
// mutex-ordered and sealed, so no read can race it and nothing acts
// on it.
type attemptEvidence struct {
	mu     sync.Mutex
	sealed bool
	msgID  string
}

// newAttemptEvidence returns an unsealed instance ready to record.
func newAttemptEvidence() *attemptEvidence { return &attemptEvidence{} }

// record is the OnAssistantMessageCreated sink for this attempt. The
// method value captures the instance itself, so a SessionAgentCall copy
// that outlives the synchronous run (a queued call drained much later by
// another goroutine) can only ever write HERE -- never into a later
// attempt's instance. Writes after resolve are dropped: resolve runs
// synchronously the moment run() returns, so any later write comes from
// an async dispatcher whose outcome is no longer this call's to
// classify.
func (e *attemptEvidence) record(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return
	}
	e.msgID = id
}

// resolve seals the attempt and returns the assistant row ID it reported
// while running. Called exactly once per attempt, synchronously after
// run() returns; the returned string is plain data owned by the calling
// goroutine from then on.
func (e *attemptEvidence) resolve() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sealed = true
	return e.msgID
}
