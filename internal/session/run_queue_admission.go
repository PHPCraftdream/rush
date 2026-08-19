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
// this pump instance. It returns a release function, an admissionEntry, and
// whether admission was granted.
//
//   - admitted=true: entry is the one just published for THIS caller's own
//     execution; release is that caller's closure.
//   - admitted=false: release is nil (a refused caller must never be able to
//     clear someone else's marker), and entry is the CURRENT holder's entry
//     — the exact one this call lost the race to — read atomically in the
//     SAME critical section as the refusal itself.
//
// The refused case used to return (nil, nil, false) and make the caller
// perform a SEPARATE waitForAdmission lookup afterward. That reopened an ABA
// race this function exists to close: between the refusal returning and the
// follow-up lookup running, the entry that caused the refusal could finish
// and release (deleting itself from p.inFlight), and a completely different
// execution for the SAME sessionID could be admitted and registered in its
// place. The follow-up lookup would then find and wait on that REPLACEMENT
// entry, silently misattributing its outcome — including its terminal
// failure, Ack failure, or lease loss — to the execution the caller actually
// lost the race to (task #587/P0-1 of the 2026-08-19 release-readiness
// follow-up review). Returning the observed entry directly, from inside the
// same lock as the refusal, leaves no window for that swap: whatever this
// call reports having lost to is provably the SAME entry a caller goes on to
// wait on.
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

	if existing, busy := p.inFlight[sessionID]; busy {
		// Read and return the CURRENT holder's entry from inside the SAME
		// critical section as the refusal itself — see this function's own
		// doc for why a separate lookup after the fact reopens an ABA race
		// (task #587/P0-1). There is no window here in which `existing`
		// could stop being the entry that caused this exact refusal.
		return nil, existing, false
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

// AdmissionEntryHandleForTest is an opaque, exported handle around the
// package-private admissionEntry, letting the external test binary
// (package session_test) hold a reference to a SPECIFIC entry across
// multiple calls and block on its completion — without exposing
// admissionEntry's internals generally.
type AdmissionEntryHandleForTest struct {
	e *admissionEntry
}

// Done returns the channel that closes when the referenced execution
// releases (see admissionEntry.done's own doc). Test-only.
func (h AdmissionEntryHandleForTest) Done() <-chan struct{} {
	return h.e.done
}

// Err returns the referenced execution's outcome. Only valid to call after
// Done() has closed — mirrors admissionEntry.err's own happens-before
// contract. Test-only.
func (h AdmissionEntryHandleForTest) Err() error {
	return h.e.err
}

// AdmitSessionObservedEntryForTest exposes admitSession's REFUSED-branch
// return value (the observed current holder's entry) to the package's
// external test binary, so task #587/P0-1's atomic-handoff contract — the
// refusal returns the SAME entry a caller goes on to wait on, with no
// separate lookup in between — can be asserted directly.
//
// Test-only seam. Nothing in production calls it.
func (p *RunQueuePump) AdmitSessionObservedEntryForTest(sessionID string) (entry AdmissionEntryHandleForTest, admitted bool) {
	_, e, ok := p.admitSession(sessionID)
	return AdmissionEntryHandleForTest{e: e}, ok
}

// SetTestAfterAdmissionRefusalForTest installs (or clears, with nil) the
// TestAfterAdmissionRefusal hook on an already-constructed pump, so a test
// can wire the hook up only once it has values (e.g. an admissionEntry
// handle, an error) that are only available after Start() and the
// background execution it is racing against have already begun. See
// RunQueuePumpConfig.TestAfterAdmissionRefusal's own doc for what the hook
// is for.
//
// Test-only seam. Nothing in production calls it.
func (p *RunQueuePump) SetTestAfterAdmissionRefusalForTest(hook func(sessionID string)) {
	p.cfg.TestAfterAdmissionRefusal = hook
}
