package sessionrun

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// Per-session admission: the single atomic gate both dispatch paths — the
// background tick's processEntry and the synchronous DrainSessionNow — must
// go through before executing anything for a session.

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

	identityMu        sync.Mutex
	assistantIDs      map[string]struct{}
	identityListeners []func(string)
	identitySealed    bool
}

func (e *admissionEntry) publishAssistantID(id string) {
	if e == nil || id == "" {
		return
	}
	e.identityMu.Lock()
	if e.identitySealed {
		e.identityMu.Unlock()
		return
	}
	if e.assistantIDs == nil {
		e.assistantIDs = make(map[string]struct{})
	}
	if _, exists := e.assistantIDs[id]; exists {
		e.identityMu.Unlock()
		return
	}
	e.assistantIDs[id] = struct{}{}
	listeners := append([]func(string){}, e.identityListeners...)
	e.identityMu.Unlock()
	for _, listener := range listeners {
		listener(id)
	}
}

func (e *admissionEntry) observeAssistantIDs(listener func(string)) {
	if e == nil || listener == nil {
		return
	}
	e.identityMu.Lock()
	ids := make([]string, 0, len(e.assistantIDs))
	for id := range e.assistantIDs {
		ids = append(ids, id)
	}
	e.identityListeners = append(e.identityListeners, listener)
	e.identityMu.Unlock()
	for _, id := range ids {
		listener(id)
	}
}

func (e *admissionEntry) sealAssistantIDs() {
	if e == nil {
		return
	}
	e.identityMu.Lock()
	e.identitySealed = true
	e.identityListeners = nil
	e.identityMu.Unlock()
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
	// right now (session.ErrCallQueuedNotExecuted / SessionLockBusyError). No row
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

	// assistantIDs are the assistant rows produced by this execution.
	assistantIDs map[string]struct{}

	// terminalAssistantID is the last assistant row produced by this execution.
	terminalAssistantID string
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
func executedOutcome(rowID string, err error, assistantIDs map[string]struct{}, terminalAssistantID string) admissionOutcome {
	outcome := admissionOutcome{rowID: rowID, kind: outcomeExecuted, err: err}
	// executeEntrySync returns an isolated snapshot, so ownership transfers
	// directly into the immutable admission outcome without a second copy.
	outcome.assistantIDs = assistantIDs
	outcome.terminalAssistantID = terminalAssistantID
	return outcome
}

func cloneAssistantIDs(ids map[string]struct{}) map[string]struct{} {
	if len(ids) == 0 {
		return nil
	}
	clone := make(map[string]struct{}, len(ids))
	for id := range ids {
		clone[id] = struct{}{}
	}
	return clone
}

type drainAssistantIdentityKey struct{}

// WithDrainAssistantIdentity captures the row produced by a drained call.
func WithDrainAssistantIdentity(ctx context.Context, capture func(map[string]struct{}, string)) context.Context {
	return context.WithValue(ctx, drainAssistantIdentityKey{}, capture)
}

func recordDrainAssistantIdentity(ctx context.Context, ids map[string]struct{}, terminalID string) {
	if capture, ok := ctx.Value(drainAssistantIdentityKey{}).(func(map[string]struct{}, string)); ok && len(ids) > 0 {
		capture(cloneAssistantIDs(ids), terminalID)
	}
}

type executionAssistantIdentityKey struct{}

// WithExecutionAssistantIdentity captures the row produced by a queue call.
func WithExecutionAssistantIdentity(ctx context.Context, capture func(string)) context.Context {
	return context.WithValue(ctx, executionAssistantIdentityKey{}, capture)
}

// ExecutionAssistantIdentityRecorder returns the direct execution's identity sink.
func ExecutionAssistantIdentityRecorder(ctx context.Context) func(string) {
	if capture, ok := ctx.Value(executionAssistantIdentityKey{}).(func(string)); ok {
		return capture
	}
	return func(string) {}
}

// WithoutExecutionAssistantIdentity detaches the sink from nested agent calls.
func WithoutExecutionAssistantIdentity(ctx context.Context) context.Context {
	return context.WithValue(ctx, executionAssistantIdentityKey{}, (func(string))(nil))
}

// RecordExecutionAssistantIdentity reports the row produced by a queue call.
func RecordExecutionAssistantIdentity(ctx context.Context, id string) {
	if capture, ok := ctx.Value(executionAssistantIdentityKey{}).(func(string)); ok && id != "" {
		capture(id)
	}
}

type drainAssistantIdentityLiveKey struct{}

// WithDrainAssistantIdentityLive observes rows as their execution creates them.
func WithDrainAssistantIdentityLive(ctx context.Context, capture func(string)) context.Context {
	return context.WithValue(ctx, drainAssistantIdentityLiveKey{}, capture)
}

func recordDrainAssistantIdentityLive(ctx context.Context, id string) {
	if capture, ok := ctx.Value(drainAssistantIdentityLiveKey{}).(func(string)); ok && id != "" {
		capture(id)
	}
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
		rel(executedOutcome("", outcome, nil, ""))
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

// Entry admission and dispatch: deciding which pending entries this pump
// instance may lease (in-flight guard, busy backoff, bounded worker pool,
// shutdown admission gate), leasing them, and spawning executeEntry.

// AlreadyAttempted is a marker interface for terminal failures.
// Errors implementing this interface (e.g., agent.ErrCallAlreadyAttempted)
// indicate that the call already left a persistent trace and retry would
// cause duplicates (task #339 regression protection).
//
// This interface is defined in the session package to avoid import cycles:
// the agent package implements it without importing session.
type AlreadyAttempted interface {
	AlreadyAttempted() bool
}

// processEntry attempts to lease and execute a single run queue entry.
func (p *RunQueuePump) processEntry(ctx context.Context, entry *session.RunQueueEntry) {
	// Reserve the session before touching the durable row at all. Refuses
	// ANY entry for a session this pump instance is already executing for —
	// see the inFlight field's doc for why (self-inflicted lease-expiry
	// race, and same-tick same-session double dispatch). Left pending; a
	// later tick retries once that execution finishes and releases it.
	//
	// This used to be a bare read here and a bare write after leasing, 100
	// lines below. That was safe only against tick()'s own single-threaded
	// loop, and DrainSessionNow is not that — see admitSession for the race
	// it left open (P1-1). Admission now covers the lease attempt too, which
	// is a few milliseconds a concurrent drain may have to wait, in exchange
	// for there being no window at all.
	releaseSession, admission, admitted := p.admitSession(entry.SessionID)
	if !admitted {
		slog.Debug("run_queue_pump: session already has an execution in flight from this pump, deferring", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Every early return below gives the session back. The one path that
	// does NOT is the successful dispatch at the end, which hands
	// releaseSession to the executeEntry goroutine that now owns it.
	//
	// Every early-return release here defaults to noRowTouched(), NOT a bare
	// nil: nothing executed for the row on these paths (no coordinator,
	// lease raced away, shutdown-in-progress nack), so there is no
	// executeEntrySync outcome to publish for a concurrent DrainSessionNow
	// that may be waiting on this admission — and nil is NOT a safe
	// stand-in for "nothing happened": it is also executeEntrySync's own
	// return value for a clean commit, so a bare nil here would be
	// classified by classifyBackgroundOutcome as a false success (task
	// #575's coordinator review). The waiter must fall through to retrying
	// admission itself, not be handed a fabricated outcome —
	// noRowTouched()'s outcomeNoRowTouched kind is what
	// classifyBackgroundOutcome maps to that fallthrough.
	//
	// ONE early-return path below (attempts-exhausted terminal-fail)
	// overrides this default explicitly via a named release variable
	// (releaseOutcome), because task #613/F3 found that path was
	// PUBLISHING THE SAME noRowTouched()/errNoExecutionAttempted default as
	// every harmless early return here, even though it just DELETED the
	// row — see that branch's own comment.
	sessionHandedOff := false
	releaseOutcome := noRowTouched()
	defer func() {
		if !sessionHandedOff {
			releaseSession(releaseOutcome)
		}
	}()

	// Refuse to lease a pending entry for a session this pump instance
	// backed off from after an session.ErrCallQueuedNotExecuted outcome — see the
	// busyBackoffUntil field's doc. Without this, the entry (immediately
	// visible again as 'pending', by design — no attempt penalty) would be
	// re-leased and re-dispatched on the very next tick, appending another
	// duplicate call to the same busy owner's mailbox.
	p.busyBackoffMu.Lock()
	until, backingOff := p.busyBackoffUntil[entry.SessionID]
	if backingOff && !p.now().Before(until) {
		delete(p.busyBackoffUntil, entry.SessionID)
		backingOff = false
	}
	p.busyBackoffMu.Unlock()
	if backingOff {
		slog.Debug("run_queue_pump: session is in local busy-backoff, deferring", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Attempt to acquire a slot from the bounded worker pool (P1-4).
	// Non-blocking: if the pool is full, leave this entry entirely
	// untouched for the next tick rather than leasing it. This prevents
	// unbounded fan-out after a process crash with a large backlog.
	select {
	case p.execSem <- struct{}{}:
		// Slot acquired, proceed below to lease and dispatch
	default:
		slog.Debug("run_queue_pump: worker pool full, deferring", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// releaseSlot is a helper for returning a slot to the pool on error paths.
	// Must only be called if a slot was acquired above.
	releaseSlot := func() {
		<-p.execSem
	}

	// Skip if attempts exceeded (unless terminal failure flag is set).
	// TerminalFailRunQueueEntry only deletes rows in 'leased' state, but an
	// attempts-exhausted entry sits in 'pending' (that's how it was scanned
	// here) — it must be leased first, or the DELETE never matches and this
	// same entry gets re-scanned and re-fails to terminal-fail on every
	// subsequent tick forever.
	if entry.Attempts >= RunQueueMaxAttempts && !entry.TerminalFailure {
		slog.Warn("run_queue_pump: entry exceeded max attempts, terminal failing",
			"id", entry.ID, "session_id", entry.SessionID, "attempts", entry.Attempts, "instance_id", p.cfg.PumpInstanceID)
		leased, err := p.cfg.Sessions.LeaseRunQueueEntry(ctx, entry.SessionID, p.cfg.PumpInstanceID, p.leaseTTL())
		if err != nil {
			slog.Error("run_queue_pump: lease for terminal-fail failed", "id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
			releaseSlot()
			return
		}
		if leased == nil {
			// Raced with another pump instance leasing/consuming it first.
			releaseSlot()
			return
		}
		if leased.ID != entry.ID {
			// LeaseRunQueueEntry claims the OLDEST PENDING entry for the
			// session, not a specific entry by ID — if another pump instance
			// raced us and already consumed the attempts-exhausted entry we
			// scanned, this lease can land on a DIFFERENT, healthy,
			// never-executed entry for the same session (e.g. a fresh call
			// queued after the scan). Terminal-failing THAT entry would
			// silently delete legitimate, unattempted work. Release it
			// unharmed (no attempt penalty — this pump did nothing wrong to
			// it) and let a future tick handle it normally.
			if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(ctx, leased.ID, p.cfg.PumpInstanceID, "released: leased entry did not match the attempts-exhausted entry scanned for terminal-fail"); nackErr != nil {
				slog.Error("run_queue_pump: release of mismatched lease failed", "id", leased.ID, "session_id", leased.SessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
			}
			releaseSlot()
			return
		}
		// task #613/F3: this row was just DELETED by TerminalFailRunQueueEntry
		// (or, if that write itself failed, its fate is unconfirmed) — NOT
		// "nothing happened". The pre-fix code published the SAME
		// errNoExecutionAttempted default every harmless early return in
		// this function uses, which a waiting DrainSessionNow mapped to
		// "loop and inspect pending work" without ever recording a failure.
		// If that waiter had already observed an earlier row's success, its
		// next empty pending scan would then report DrainComplete — exit 0
		// over a row that was actually discarded to dead-letter. Publish a
		// row-scoped terminalDeletedOutcome instead: termErr nil means the
		// DELETE itself succeeded (still a real failure — see
		// outcomeTerminalDeleted's own doc), non-nil means even the
		// terminal write failed, leaving the row's fate unconfirmed rather
		// than silently swallowed.
		termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(ctx, leased.ID, p.cfg.PumpInstanceID)
		if termErr != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "session_id", leased.SessionID, "err", termErr, "instance_id", p.cfg.PumpInstanceID)
		}
		releaseOutcome = terminalDeletedOutcome(leased.ID, termErr)
		releaseSlot()
		return
	}

	// Skip if no coordinator (pump in scan-only mode)
	if p.cfg.Coordinator == nil {
		slog.Debug("run_queue_pump: no coordinator, skipping execution", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		releaseSlot()
		return
	}

	// Attempt to lease the entry
	leased, err := p.cfg.Sessions.LeaseRunQueueEntry(ctx, entry.SessionID, p.cfg.PumpInstanceID, p.leaseTTL())
	if err != nil {
		slog.Error("run_queue_pump: lease failed", "id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		releaseSlot()
		return
	}
	if leased == nil {
		// Another pump leased it between the scan and our attempt
		releaseSlot()
		return
	}

	// Refuse to start a new worker once shutdown has begun, and register
	// the one that IS started with workerWg — both under admitMu, so the
	// check and the Add are one atomic operation relative to Stop()'s own
	// critical section (see admitMu/stopping's doc: an unsynchronized
	// check-then-Add here raced Stop()'s cancel+Wait sequence and could
	// either panic ("Add called concurrently with Wait") or let a new
	// worker start after Stop() had already told its caller shutdown was
	// safe — undoing the fix this task exists to make).
	p.admitMu.Lock()
	if p.stopping {
		p.admitMu.Unlock()
		// The session reservation is given back by this function's own
		// deferred release (sessionHandedOff is still false here), so no
		// explicit clear is needed — and, unlike the open-coded delete this
		// replaced, it cannot clear a mark belonging to some other execution.
		//
		// Release the lease immediately instead of leaving the row
		// "leased" for up to a full leaseTTL: any pump instance (this
		// process on restart, or a different live process) can then pick
		// it up as soon as it next ticks. No attempt penalty — this pump
		// never actually attempted it. Use an independent, short-lived
		// context rather than p.ctx: p.cancel() may already have fired or
		// be about to, and this write must not be lost to that same race.
		nackCtx, nackCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(nackCtx, leased.ID, p.cfg.PumpInstanceID, "run_queue_pump: shutting down, releasing lease without executing"); nackErr != nil {
			slog.Warn("run_queue_pump: release-on-shutdown nack failed, entry will recover via lease expiry", "id", leased.ID, "session_id", leased.SessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
		}
		nackCancel()
		releaseSlot()
		return
	}
	p.workerWg.Add(1)
	p.admitMu.Unlock()

	// Execute the call (detached, not blocking this pump tick).
	//
	// p.ctx, NOT the tick's ctx. The context threaded through processEntry
	// is a SCAN context with a 5-second deadline (see tick()), sized for DB
	// reads. Now that executeEntrySync actually honours its context
	// parameter (P0-2 of the 2026-08-18 release-readiness review), passing
	// the scan context here would cap every durable turn at five seconds.
	// The execution parent has to be the pump's own lifetime, so that Stop()
	// ends it and nothing else does.
	sessionHandedOff = true
	go p.executeEntry(p.ctx, leased, releaseSession, admission)
}

// executeEntry runs a leased entry and handles success/failure. Called
// detached (go executeEntry(...)) by the background tick's processEntry,
// which has already reserved this call's execSem slot, its workerWg
// registration and its session admission before dispatching — this releases
// all three via defer regardless of outcome, matching the admission it
// assumes was already granted.
//
// releaseSession is processEntry's admitSession closure, handed over rather
// than re-derived: only the caller that was admitted may clear the marker
// (see admitSession), and after this handoff that caller is this goroutine.
// It now takes a typed admissionOutcome carrying the executeEntrySync
// outcome AND leased.ID (task #575 of the 2026-08-19 release-readiness
// review, row identity added by task #613 of the 2026-08-20 read-only
// release review): DrainSessionNow, when it loses the admission race
// against this goroutine, waits on the admissionEntry this release call
// publishes and must be able to read the same outcome — AND the same row
// ID — this function itself observed, or it has no way to distinguish a
// committed turn from a terminal failure, a failed Ack, or a lost lease
// (all four used to be indistinguishable from outside this goroutine), and
// no way to later supersede an observed failure with a same-row retry
// success instead of stranding it under a synthetic, unclearable key (task
// #613/F4).
func (p *RunQueuePump) executeEntry(ctx context.Context, leased *session.RunQueueEntry, releaseSession func(outcome admissionOutcome), admission *admissionEntry) {
	defer p.workerWg.Done()

	// Release the semaphore slot when execution completes (P1-4).
	// This guarantees the slot is returned even on panic or any error path.
	defer func() { <-p.execSem }()

	// execErr is set by the ordinary return path below; the deferred
	// recover just below can also set it if executeEntrySync panics. Either
	// way, releaseSession is always the LAST thing this goroutine does,
	// guaranteeing admission is released — and a waiting DrainSessionNow
	// unblocked — even on a panic, exactly as the pre-#575 unconditional
	// `defer releaseSession()` guaranteed before releaseSession needed an
	// outcome argument to pass. A panic here would otherwise leave
	// p.inFlight[sessionID] permanently marked busy for the rest of this
	// pump instance's lifetime, AND strand any concurrent DrainSessionNow
	// call forever on otherEntry.done — a panic must not be able to hang a
	// caller that did nothing wrong.
	var execErr error
	var assistantIDs map[string]struct{}
	var terminalAssistantID string
	defer func() {
		if r := recover(); r != nil {
			releaseSession(executedOutcome(leased.ID, fmt.Errorf("run_queue_pump: executeEntrySync panicked: %v", r), nil, ""))
			panic(r) // preserve normal panic propagation/crash behavior
		}
		releaseSession(executedOutcome(leased.ID, execErr, assistantIDs, terminalAssistantID))
	}()

	// executeEntrySync's return value already carries everything any
	// interested party could need to know about this row's outcome — see
	// its own doc. This detached background path has nobody of its own to
	// report to (every outcome that CAN be written for the row already was,
	// inside executeEntrySync), but a concurrent DrainSessionNow for the
	// SAME session may be parked on this admission's done channel waiting
	// for exactly this value, so it is always published via releaseSession,
	// never silently discarded.
	assistantIDs, terminalAssistantID, execErr = p.executeEntrySync(ctx, leased, admission)
}
