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
	// done is closed exactly once, after outcome has been written, by
	// whichever release closure admitSession handed out for this session.
	// Closing it (rather than e.g. a sync.WaitGroup) lets any number of
	// waiters observe completion via select alongside their own ctx.Done(),
	// so a caller waiting on someone else's execution is never stuck if its
	// own context ends first.
	done chan struct{}

	// outcome is the terminal outcome of the admitted execution, valid to
	// read only after done is closed — done's close is the happens-before
	// edge, exactly like a context's Done/Err pair, so outcome itself needs
	// no separate synchronization. See admissionOutcome's own doc for what
	// it carries and why a bare error is not enough (task #613 of the
	// 2026-08-20 read-only release review, F3/F4): the row-scoped
	// `{rowID, kind, err}` shape is what lets a terminal deletion be
	// published as a row-scoped failure instead of indistinguishable from
	// "nothing happened", and lets an observed outcome be recorded under
	// its OWN row ID instead of a synthetic one a later same-row success
	// can never clear.
	outcome admissionOutcome
}

// outcomeKind classifies what admitSession's admitted caller actually did
// with the session before releasing it — the piece task #613 (F3/F4 of the
// 2026-08-20 read-only release review) found missing from the pre-existing
// bare `err error` handoff. A bare error can express "something went wrong"
// but not WHICH of these fundamentally different situations produced it,
// and two of them (outcomeNoRowTouched and outcomeTerminalFailed) used to be
// published through the exact same errNoExecutionAttempted sentinel even
// though one is a harmless early return and the other is a destructive,
// row-scoped DELETE.
type outcomeKind int

const (
	// outcomeNoRowTouched means the admitted caller held the session's
	// admission slot but never durably touched any row — an early-return
	// path (busy-backoff, worker-pool-full, a raced lease, no-coordinator
	// scan mode, a shutdown-in-progress nack, DrainSessionNow's own
	// "nothing pending" bottom path, or a pre-execution DB/ctx failure). No
	// row identity to report; a waiter must retry admission itself.
	outcomeNoRowTouched outcomeKind = iota

	// outcomeExecuted means executeEntrySync actually ran the row and
	// reached SOME resolution — success, a terminal AlreadyAttempted
	// failure, ErrTurnCommitFailed, errLeaseLost, or an ordinary retryable
	// failure. rowID is always set; err is nil only for a clean commit.
	outcomeExecuted

	// outcomeTerminalDeleted means a row was leased and terminal-failed
	// (DELETEd) WITHOUT ever calling executeEntrySync — the
	// attempts-exhausted fast path in processEntry and DrainSessionNow's
	// mirror of it. Distinguishing this from outcomeNoRowTouched is the
	// core of task #613/F3: this is destructive, row-scoped bookkeeping
	// that a waiter must learn about, even though no model/tool call was
	// ever attempted. rowID is always set. err is nil when the terminal
	// write itself succeeded (still a real, row-scoped failure — the row
	// this outcome is ABOUT ended in dead-letter, not a nil meaning
	// "nothing happened"); non-nil when the terminal write ITSELF failed,
	// in which case the row's fate is unconfirmed, not "no-op" (F3's second
	// bad branch).
	outcomeTerminalDeleted

	// outcomeBusy means a genuinely different, live owner holds the session
	// right now (ErrCallQueuedNotExecuted / SessionLockBusyError). No row
	// was touched by THIS caller. Kept distinct from outcomeNoRowTouched
	// only insofar as classifyBackgroundOutcome's stopNow semantics depend
	// on it — see that function's own doc.
	outcomeBusy
)

// admissionOutcome is the typed replacement for admitSession's release
// closure's former bare `error` parameter (task #613 of the 2026-08-20
// read-only release review, findings F3 and F4). The root cause both
// findings shared: admissionEntry published only an error, with no row
// identity and no outcome kind, so a destructive terminal deletion was
// indistinguishable from a harmless early return (F3), and an observed
// outcome had no row ID a later same-row success could use to clear it
// (F4, forcing every observed failure into a synthetic, never-clearable
// `__unattributed_N` ledger key even when the row ID was perfectly known).
type admissionOutcome struct {
	// rowID is the durable run_queue row this outcome is ABOUT, when known.
	// Empty for outcomeNoRowTouched and outcomeBusy (no row was touched by
	// this caller). Always set for outcomeExecuted and outcomeTerminalDeleted.
	rowID string

	// kind classifies what happened — see outcomeKind's own doc.
	kind outcomeKind

	// err is the specific error, if any. Its meaning depends on kind: for
	// outcomeExecuted, nil means a clean commit (mirrors executeEntrySync's
	// own nil-on-success return); for outcomeTerminalDeleted, nil means the
	// terminal DELETE itself succeeded (the row is STILL a failure — see
	// outcomeTerminalDeleted's own doc — nil here is not "nothing
	// happened"); non-nil for outcomeTerminalDeleted means the terminal
	// write itself failed, leaving the row's fate unconfirmed.
	err error
}

// noRowTouched builds the outcome every early-return admission release must
// publish: no durable row was touched, so a waiter must retry admission
// itself. This replaces the bare errNoExecutionAttempted sentinel at call
// sites, though that sentinel's error identity is preserved (see its own
// doc) for any caller still matching on it directly.
func noRowTouched() admissionOutcome {
	return admissionOutcome{kind: outcomeNoRowTouched, err: errNoExecutionAttempted}
}

// busyOutcome builds the outcome an admission release must publish when a
// genuinely different, live owner holds the session right now.
func busyOutcome(err error) admissionOutcome {
	return admissionOutcome{kind: outcomeBusy, err: err}
}

// executedOutcome builds the outcome an admission release must publish
// after executeEntrySync actually ran rowID to some resolution.
func executedOutcome(rowID string, err error) admissionOutcome {
	return admissionOutcome{rowID: rowID, kind: outcomeExecuted, err: err}
}

// terminalDeletedOutcome builds the outcome an admission release must
// publish when rowID was terminal-failed (deleted) WITHOUT ever calling
// executeEntrySync. termErr is the terminal DELETE's own error, if the
// write itself failed (nil means the write succeeded and the row is a
// confirmed dead-letter) — see outcomeTerminalDeleted's own doc.
func terminalDeletedOutcome(rowID string, termErr error) admissionOutcome {
	return admissionOutcome{rowID: rowID, kind: outcomeTerminalDeleted, err: termErr}
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
func (p *RunQueuePump) admitSession(sessionID string) (release func(outcome admissionOutcome), entry *admissionEntry, admitted bool) {
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
	return func(outcome admissionOutcome) {
		once.Do(func() {
			e.outcome = outcome
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
// The returned release closure keeps taking a plain `error` (not the
// internal admissionOutcome type) so pre-existing tests calling
// release(nil)/release(someErr) keep compiling unchanged — this seam wraps
// the value into an executedOutcome with no row ID, which is an accurate
// stand-in for "some outcome happened" for every existing caller of this
// seam, none of which assert on outcome kind or row ID.
//
// Test-only seam. Nothing in the production paths calls it; they call
// admitSession, which this forwards to unchanged.
func (p *RunQueuePump) AdmitSessionForTest(sessionID string) (release func(outcome error), admitted bool) {
	rel, _, ok := p.admitSession(sessionID)
	if rel == nil {
		return nil, ok
	}
	return func(outcome error) {
		rel(executedOutcome("", outcome))
	}, ok
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

// Err returns the referenced execution's outcome error. Only valid to call
// after Done() has closed — mirrors admissionEntry.outcome's own
// happens-before contract. Test-only.
func (h AdmissionEntryHandleForTest) Err() error {
	return h.e.outcome.err
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
