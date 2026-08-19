// Per-session admission: the single atomic gate both dispatch paths — the
// background tick's processEntry and the synchronous DrainSessionNow — must
// go through before executing anything for a session.

package session

import "sync"

// admissionEntry is the shared record one admitted execution publishes for
// anyone else asking about the same session while it runs. It exists so a
// caller that loses the admission race — DrainSessionNow meeting a
// background worker already inFlight for the session — can wait for that
// SPECIFIC execution's outcome and interpret it, instead of only learning
// that admission eventually cleared and assuming a continuation completed
// (task #575 of the 2026-08-19 release-readiness review).
//
// Before this type existed, p.inFlight was a bare map[string]struct{}: it
// answered "is someone executing this session right now?" but had no way to
// answer "and what did they get?". DrainSessionNow's losing branch treated
// "admission cleared" as synonymous with "a continuation completed here",
// which is only true for ONE of (at least) five possible outcomes — see
// classifyBackgroundOutcome in run_queue_drain_session.go.
type admissionEntry struct {
	// done is closed exactly once, after err has been written, by whichever
	// release closure admitSession handed out for this session. Closing it
	// (rather than e.g. a sync.WaitGroup) lets any number of waiters observe
	// completion via select alongside their own ctx.Done(), so a caller
	// waiting on someone else's execution is never stuck if its own context
	// ends first.
	done chan struct{}

	// err is the terminal outcome of the admitted execution, valid to read
	// only after done is closed — done's close is the happens-before edge,
	// exactly like a context's Done/Err pair, so err itself needs no
	// separate synchronization. It is either:
	//
	//   - whatever executeEntrySync returned for THIS row, if the admitted
	//     caller actually called it: nil for an acked success, or one of
	//     ErrCallQueuedNotExecuted, *SessionLockBusyError,
	//     ErrTurnCommitFailed, errLeaseLost, an AlreadyAttempted-wrapping
	//     terminal failure, or an ordinary retryable failure; or
	//   - errNoExecutionAttempted, if the admitted caller held this slot but
	//     never called executeEntrySync at all (an early-return path — see
	//     that sentinel's own doc for the full enumeration).
	//
	// The two must never be conflated: nil is ALSO executeEntrySync's own
	// "clean commit" return value, so a release call that has nothing
	// executed to report must publish errNoExecutionAttempted, never a bare
	// nil — a caller waiting on this entry cannot otherwise distinguish "a
	// continuation committed" from "nothing happened here, go look
	// elsewhere" (the hole task #575's coordinator review found and this
	// sentinel closes).
	//
	// A caller that runs executeEntrySync itself (DrainSessionNow's own
	// leasing branch, or processEntry's background dispatch) does not need
	// to read this field for ITS OWN outcome — it already has execErr
	// directly. This field matters only to a SECOND caller observing the
	// FIRST caller's execution, and both DrainSessionNow branches funnel
	// their outcome (self-executed or observed-via-wait) through the same
	// classifyBackgroundOutcome helper, so the two paths cannot disagree
	// about what a given error means.
	err error
}

// admitSession atomically reserves sessionID for exactly one execution by
// this pump instance. It returns a release function, the admissionEntry that
// was just published for it, and whether admission was granted; release and
// entry are nil when admission was refused — see waitForAdmission to obtain
// the CURRENT holder's entry in that case.
//
// This exists because a plain "check the map, then later write the map" is
// only safe when there is a single sequential caller, and there are two
// (P1-1 of the 2026-08-18 release-readiness review). processEntry checked
// inFlight early and marked it after leasing, which tick()'s single-threaded
// loop made safe against itself. DrainSessionNow leased first and then
// assigned the same key unconditionally, from a different goroutine. So a
// background worker executing row A for session S and a drain executing row
// B for the same S both believed one boolean represented them, and the first
// to finish deleted it — after which the next tick or drain saw S as free
// while an execution was still running. The OS session lock usually stopped
// two turns from reaching the model at once, so the visible damage was
// spurious lease/Nack cycles, wrong drain outcomes and a pump that looked
// idle during shutdown while it was not.
//
// Two properties fix that, and both are enforced here rather than by
// convention at the call sites:
//
//   - Check and mark are ONE operation under the mutex, so there is no
//     window between deciding a session is free and claiming it.
//   - Only the caller that was admitted can clear the marker, because the
//     only way to clear it is the closure returned to that caller. A second
//     execution cannot delete the first one's mark, because it never gets a
//     mark of its own — it is refused admission instead.
//
// Admission is exclusive, not a refcount. One execution per session per pump
// instance is the invariant the inFlight guard always intended; DrainSessionNow
// was bypassing it, not implementing a second legitimate mode.
//
// release is idempotent: call sites hand it between functions (processEntry
// hands it to the executeEntry goroutine it spawns) and a double call on an
// error path must not free a slot that a later, unrelated execution has
// since taken. Only the FIRST call's outcome argument is recorded into
// entry.err before entry.done is closed — later calls (e.g. a deferred
// release racing an explicit one on a shared error path) contribute nothing
// further, exactly like the previous struct{}-map version's once-only
// delete.
func (p *RunQueuePump) admitSession(sessionID string) (release func(outcome error), entry *admissionEntry, admitted bool) {
	p.inFlightMu.Lock()
	defer p.inFlightMu.Unlock()

	if _, busy := p.inFlight[sessionID]; busy {
		return nil, nil, false
	}
	e := &admissionEntry{done: make(chan struct{})}
	p.inFlight[sessionID] = e

	var once sync.Once
	return func(outcome error) {
		once.Do(func() {
			e.err = outcome
			close(e.done)

			p.inFlightMu.Lock()
			if p.inFlight[sessionID] == e {
				delete(p.inFlight, sessionID)
			}
			p.inFlightMu.Unlock()
		})
	}, e, true
}

// waitForAdmission returns the admissionEntry currently published for
// sessionID, if any — the entry a concurrent admitSession call for the same
// session lost the race against. Returns found=false once nothing is
// currently admitted for the session (the caller should retry admitSession
// itself in that case, not assume anything about what happened while it was
// busy).
//
// Read-only, and deliberately not a substitute for admitSession's
// check-and-mark: this only lets an ALREADY-REFUSED caller find the specific
// entry to wait on. A caller that has not yet attempted admission must still
// go through admitSession, or it recreates the same "check, then act" gap
// admitSession itself exists to close.
func (p *RunQueuePump) waitForAdmission(sessionID string) (entry *admissionEntry, found bool) {
	p.inFlightMu.Lock()
	defer p.inFlightMu.Unlock()
	e, ok := p.inFlight[sessionID]
	return e, ok
}

// AdmitSessionForTest exposes admitSession to the package's external test
// binary (package session_test), so the gate's two properties — exclusivity,
// and that only the admitted caller can clear the marker — can be asserted
// directly instead of only inferred from a race that has to be provoked.
//
// Test-only seam. Nothing in the production paths calls it; they call
// admitSession, which this forwards to unchanged.
func (p *RunQueuePump) AdmitSessionForTest(sessionID string) (release func(outcome error), admitted bool) {
	rel, _, ok := p.admitSession(sessionID)
	return rel, ok
}
