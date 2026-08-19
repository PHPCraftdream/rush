// Per-session admission: the single atomic gate both dispatch paths — the
// background tick's processEntry and the synchronous DrainSessionNow — must
// go through before executing anything for a session.

package session

import "sync"

// admitSession atomically reserves sessionID for exactly one execution by
// this pump instance. It returns a release function and whether admission
// was granted; release is nil when it was not.
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
// since taken.
func (p *RunQueuePump) admitSession(sessionID string) (release func(), admitted bool) {
	p.inFlightMu.Lock()
	defer p.inFlightMu.Unlock()

	if _, busy := p.inFlight[sessionID]; busy {
		return nil, false
	}
	p.inFlight[sessionID] = struct{}{}

	var once sync.Once
	return func() {
		once.Do(func() {
			p.inFlightMu.Lock()
			delete(p.inFlight, sessionID)
			p.inFlightMu.Unlock()
		})
	}, true
}

// There is deliberately no exported "is this session busy?" predicate.
// A read-only check is only ever the first half of a check-then-act, and
// that split is precisely what P1-1 was. Callers that need the session take
// admitSession's answer; callers that need to wait retry it.

// AdmitSessionForTest exposes admitSession to the package's external test
// binary (package session_test), so the gate's two properties — exclusivity,
// and that only the admitted caller can clear the marker — can be asserted
// directly instead of only inferred from a race that has to be provoked.
//
// Test-only seam. Nothing in the production paths calls it; they call
// admitSession, which this forwards to unchanged.
func (p *RunQueuePump) AdmitSessionForTest(sessionID string) (release func(), admitted bool) {
	return p.admitSession(sessionID)
}
