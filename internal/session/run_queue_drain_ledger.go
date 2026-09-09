package session

// The drain's verdict machinery: DrainResult and the per-row ledger behind
// it. The ledger exists because a drain's verdict is not a count -- a later
// row committing cleanly must never cancel out an earlier row this call
// cannot vouch for, so the outcome is tracked per row identity. Split out of
// run_queue_drain_session.go when the 1000-line file limit landed.

import (
	"context"
	"fmt"
	"strings"
)

// DrainResult is the session-scoped outcome of one DrainSessionNow call —
// what happened across every durable row it touched, not any single row's
// own result. It exists (task #592/P0-1 of the 2026-08-19 release-readiness
// second follow-up review) because a plain (drained bool, err error) pair
// cannot express "some rows ran, and at least one of them failed or is
// unconfirmed" without either losing that failure (if a later row's success
// is allowed to overwrite it) or losing the fact that OTHER rows genuinely
// committed (if the loop stops at the first error). Both of those are
// exactly the shapes task #575/#588/#578 fixed for the SAME row retrying —
// see this file's CONTRACT DECISION comment on DrainSessionNow — and this
// type is what makes the equivalent mistake for a DIFFERENT row impossible
// to write by construction, rather than merely checked at each call site.
//
// The four values are deliberately exhaustive and mutually exclusive:
//
//   - DrainNoWork: nothing executed in this call, and no ROW ever reached a
//     resolution this call could attribute to it (contrast DrainFailed,
//     which requires a resolution the ledger could tie to something). This
//     is the ONLY value under which a caller may leave an original
//     cancellation/error standing without inspecting err further — it means
//     "there was nothing for this call to do", not "everything succeeded".
//     err is NOT always nil here (see DrainNoWork's own doc below): a few
//     early-exit paths (this call's own ctx already done, a DB lease attempt
//     itself failing, this call's own ctx ending before an execution slot
//     was available, or this call declining to start because the pump is
//     stopping) report DrainNoWork with a non-nil err describing WHY this
//     call itself did not run anything — as opposed to DrainFailed's err,
//     which always describes a ROW's outcome. Every caller today (the sole
//     production one, app_run.go's drainOutcomeError) already treats
//     DrainNoWork as "leave the original error standing" unconditionally,
//     without reading err off this pairing at all, so this asymmetry is not
//     presently observable — but a future caller inspecting err directly
//     must not assume it is nil just because result is DrainNoWork.
//   - DrainComplete: this call executed at least one row, and every row it
//     is able to vouch for resolved as a genuine, confirmed commit. This is
//     the ONLY value a caller may read as "the continuation fully
//     completed" and use to replace an original failure with success. See
//     err's own doc below for why "confirmed" excludes the cross-process
//     re-check outcomes even when they are not literally an error the
//     operator needs to act on.
//   - DrainPartial: this call executed at least one row that committed
//     cleanly, but the session became busy/contended (a genuinely
//     different live owner) before every pending row could be attempted.
//     Some work happened; some real, still-pending work did not.
//   - DrainFailed: this call executed at least one row, and at least one
//     row it touched ended in a failure this call cannot vouch as
//     resolved — a terminal AlreadyAttempted loss, a failed Ack leaving
//     the row leased, a lost lease handed to an unknown new owner, an
//     ordinary retryable failure whose row this call never got back
//     around to, or a cross-process outcome this call's re-check could
//     not confirm as a commit. A DIFFERENT, later row committing cleanly
//     in the SAME call does NOT downgrade this to DrainComplete or
//     DrainPartial — see rowLedger's own doc for the identity rule that
//     enforces this.
type DrainResult int

const (
	// DrainNoWork means nothing executed and no row's outcome is on record —
	// the pre-existing, still-correct "nothing to drain" contract every
	// caller already depends on. err is OFTEN nil here (the pure-contention
	// paths — see e.g. rowLedger.verdict's own DrainNoWork branch), but NOT
	// ALWAYS: several early-exit sites in DrainSessionNow return DrainNoWork
	// paired with a non-nil err that describes why THIS CALL stopped before
	// touching anything (its own ctx already done, its own lease attempt
	// failing at the DB layer, its own ctx ending before an execution slot
	// opened up, or this call declining to start because the pump is
	// stopping) — see verdictOnCtxDone's doc and the three
	// `if result == DrainNoWork { return DrainNoWork, <call-scoped err> }`
	// sites in DrainSessionNow itself. Every one of those errs is about this
	// CALL, never about a row, which is what keeps DrainNoWork distinct from
	// DrainFailed (whose err is always row-scoped). The sole production
	// consumer (app_run.go's drainOutcomeError) reads DrainNoWork as "let the
	// caller's own original error stand" and does not inspect err on this
	// pairing at all, so this has no observable effect today — documented
	// here so a future caller that DOES read err off a DrainNoWork result
	// does not assume nil.
	DrainNoWork DrainResult = iota
	// DrainComplete means every row this call executed or waited on
	// resolved as a genuine, confirmed commit. err is always nil when
	// result is DrainComplete — this is the ONLY (result, err) pairing a
	// caller may treat as full success.
	DrainComplete
	// DrainPartial means at least one row committed cleanly, but a
	// genuinely different live owner made the session busy/contended
	// before this call could reach every pending row. err is always
	// ErrDrainIncomplete when result is DrainPartial.
	DrainPartial
	// DrainFailed means at least one row this call touched ended in a
	// failure or an unconfirmed outcome that survives to the end of this
	// call, regardless of what any OTHER row did. err carries the specific
	// cause — never nil when result is DrainFailed.
	DrainFailed
)

// String renders DrainResult for logs and test failure messages.
func (r DrainResult) String() string {
	switch r {
	case DrainNoWork:
		return "no-work"
	case DrainComplete:
		return "complete"
	case DrainPartial:
		return "partial"
	case DrainFailed:
		return "failed"
	default:
		return fmt.Sprintf("DrainResult(%d)", int(r))
	}
}

// Executed reports whether this call ran (or observed someone else run) at
// least one row — i.e. result is anything other than DrainNoWork. Kept as a
// named predicate, not a raw comparison, so call sites read as intent
// ("did anything happen here") rather than an enum-ordering assumption.
func (r DrainResult) Executed() bool {
	return r != DrainNoWork
}

// rowLedger is DrainSessionNow's session-scoped accumulator. It replaces the
// pre-#592 shape — a single named-return `err` unconditionally overwritten
// by every classified outcome, with a `lastErrRowID` string bolted on only
// for the bottom-of-loop re-check — with one rule, enforced structurally
// rather than by convention at each call site:
//
// A later outcome may only supersede an earlier FAILURE if it is a later
// resolution FOR THE SAME ROW. A different row's outcome — success or
// failure — never clears an earlier row's unresolved failure; it can only
// ever ADD to the ledger's terminal verdict, never subtract from it.
//
// This is what the two-value (drained, err) shape could not express: err
// was simultaneously "the current row's outcome" and "the whole call's
// verdict", and nothing tied it to WHICH row it was about, so a later row's
// nil silently read as "the earlier row's failure is now resolved" even
// though the two rows have nothing to do with each other. rowLedger makes
// that misreading impossible to write: failed rows accumulate in a map
// keyed by ID, and the only removal path (recordSuccess) requires the
// caller to name the exact row being resolved.
type rowLedger struct {
	// anyExecuted is set the first time any row in this call reaches a
	// genuine resolution (executed locally, or observed via a wait) —
	// drives DrainNoWork vs. everything else. Once true, never reset: even
	// a row that later resolves cleanly still means "something happened in
	// this call", which is DrainComplete/Partial/Failed territory, never
	// back to DrainNoWork.
	anyExecuted bool

	// failed holds one entry per row this call currently believes ended in
	// a failure or unconfirmed outcome, keyed by row ID. A row is removed
	// from this map ONLY by recordSuccess(rowID) — i.e. a later attempt AT
	// THAT SAME ROW ID committing cleanly. Nothing else ever deletes an
	// entry: not a different row's success, not the loop reaching the
	// bottom, not the busy/contended stopNow path. This is the field that
	// makes "row A terminal-fails, row B commits" report DrainFailed,
	// because B's success touches failed["B"] (a no-op — B was never in
	// the map) and leaves failed["A"] exactly as it was.
	//
	// A row with no assigned ID (the observed-admission branch's outcome
	// for a wait that resolved with a failure this call cannot attribute
	// to a specific row it leased itself) is recorded under a synthetic,
	// per-occurrence key so distinct unattributed failures don't collide —
	// see recordUnattributed.
	failed map[string]error

	// order records failure-key insertion order (including duplicate
	// inserts of the same key, e.g. a row that fails, is superseded, then
	// fails again under a different cause before this loop moves on — rare
	// but not impossible), so mostRecentFailure can deterministically pick
	// the freshest surviving entry without depending on Go's randomized
	// map iteration order. A key's appearance in order after its entry has
	// been deleted from failed (via recordSuccess) is harmless dead
	// weight: mostRecentFailure only reports keys still present in failed.
	order []string

	// unattributedSeq counts synthetic keys minted by recordUnattributed.
	unattributedSeq int
}

// newRowLedger returns an empty ledger — the DrainNoWork starting state.
func newRowLedger() *rowLedger {
	return &rowLedger{failed: make(map[string]error)}
}

// unattributedFailureKeyPrefix is the prefix of every synthetic key minted
// by recordUnattributed. mostRecentFailure uses it to distinguish a
// row-attributed failure entry (keyed by the leased row's own ID) from a
// general, unattributed one, so the ledger's representative-error priority
// rule (task #624/F-7) can rank the two tiers without a second map.
const unattributedFailureKeyPrefix = "__unattributed_"

// isUnattributedFailureKey reports whether key was minted by
// recordUnattributed rather than carrying a leased row's own ID.
func isUnattributedFailureKey(key string) bool {
	return strings.HasPrefix(key, unattributedFailureKeyPrefix)
}

// recordSuccess marks rowID as having reached a genuine, confirmed commit —
// clearing any prior failure recorded for THAT SAME rowID, and no other. An
// empty rowID (the observed-admission branch's success case, which has no
// row identity of its own to clear — see DrainSessionNow's call site) is
// accepted and simply records that something executed, without touching
// the failed map at all.
func (l *rowLedger) recordSuccess(rowID string) {
	l.anyExecuted = true
	if rowID != "" {
		delete(l.failed, rowID)
	}
}

// RecordFailure marks rowID as ending in outcomeErr — a failure or
// unconfirmed outcome that survives until (and unless) a LATER resolution
// for the exact same rowID clears it via recordSuccess. rowID must be
// non-empty and stable across an ordinary same-process retry of that row
// (the leased entry's own ID) so a later retry's recordSuccess can find and
// clear it. If outcomeErr is nil, it is substituted with a sentinel error.
func (l *rowLedger) recordFailure(rowID string, outcomeErr error) {
	l.anyExecuted = true
	if outcomeErr == nil {
		outcomeErr = fmt.Errorf("%w (row=%s)", ErrDrainFailureUnspecified, rowID)
	}
	l.failed[rowID] = outcomeErr
	l.order = append(l.order, rowID)
}

// RecordUnattributed marks a failure this call observed (via a wait on
// someone else's admissionEntry, or a failure on this call's own return
// path after an earlier row already executed) but cannot — or need not —
// tie to a row ID it leased itself. Each call mints a distinct synthetic
// key, because an unattributed failure can never be resolved by a later
// recordSuccess (there is no shared ID to match against) and must
// therefore never collide with — or be silently dropped by — a different
// unattributed failure observed later in the same loop. If outcomeErr is
// nil, it is substituted with a sentinel error.
func (l *rowLedger) recordUnattributed(outcomeErr error) {
	l.anyExecuted = true
	if outcomeErr == nil {
		outcomeErr = fmt.Errorf("%w (unattributed)", ErrDrainFailureUnspecified)
	}
	l.unattributedSeq++
	key := fmt.Sprintf("%s%d", unattributedFailureKeyPrefix, l.unattributedSeq)
	l.failed[key] = outcomeErr
	l.order = append(l.order, key)
}

// verdict resolves the ledger into DrainSessionNow's final (DrainResult,
// error) pair. contended is true when the loop is stopping because a
// genuinely different live owner holds the session right now (the stopNow
// outcomes) rather than because there is nothing left pending.
func (l *rowLedger) verdict(contended bool) (DrainResult, error) {
	if !l.anyExecuted {
		// Nothing ran and nothing failed. contended may still be true here
		// (the session was busy from the very first row this call looked
		// at) — that is the pre-existing, unaffected DrainNoWork contract:
		// see DrainSessionNow's own doc for why this case is deliberately
		// left alone.
		return DrainNoWork, nil
	}
	if len(l.failed) > 0 {
		// At least one row this call touched has never been superseded by
		// a LATER resolution at that same row. Report ONE representative
		// error — the most recently recorded failure still present in the
		// ledger, so the error a caller sees corresponds to the freshest
		// information this call has. errors.Join across every surviving
		// failure was considered and rejected: callers already treat "any
		// failure present" as actionable via errors.Is/As against a
		// SPECIFIC sentinel, and joining would make every such check
		// depend on errors.Join's wrapping behavior instead of a plain
		// errors.Is chain.
		repErr := l.mostRecentFailure()
		if repErr == nil {
			// Safety net: invariant violation if failures exist but none
			// survives the order walk, or a stored value is nil.
			return DrainFailed, fmt.Errorf("%w: ledger holds %d failure(s) but no representative error", ErrDrainFailureUnspecified, len(l.failed))
		}
		return DrainFailed, repErr
	}
	if contended {
		// Every row this call actually touched resolved cleanly, but a
		// genuinely different live owner made the session busy/contended
		// before this call could reach every pending row — some real work
		// was left untouched. See ErrDrainIncomplete's own doc.
		return DrainPartial, ErrDrainIncomplete
	}
	return DrainComplete, nil
}

// hasFailures reports whether the ledger currently holds any surviving
// failure entry (i.e. verdict(...) would resolve to DrainFailed regardless
// of contended). Used by DrainSessionNow's terminal outstanding-row check
// (task #610) to avoid recording a REDUNDANT, more-recently-inserted
// unattributed entry for a row the ledger already has a specific,
// identity-tied failure for -- see that check's own comment for why
// querying HasOutstandingRunQueueEntriesForSession is skipped entirely once
// a real failure is already on record: it cannot change DrainFailed into
// anything else, and mostRecentFailure() picking the freshest SURVIVING
// entry would otherwise let a generic "still outstanding" message shadow
// the specific, already-diagnosed cause (e.g. ErrTurnCommitFailed,
// errLeaseLost) a caller and its tests depend on seeing.
func (l *rowLedger) hasFailures() bool {
	return len(l.failed) > 0
}

// mostRecentFailure returns the error the ledger should report as its
// representative failure, chosen by a two-tier priority rule (task #624,
// F-7 of the 2026-08-20 ninth review):
//
//   - Tier 1, ROW-ATTRIBUTED entries (recordFailure under a leased row's own
//     ID): these carry a classified, row-identity-tied cause — a terminal
//     AlreadyAttempted loss, ErrTurnCommitFailed, errLeaseLost, an ambiguous
//     ErrRowOutcomeUnconfirmed re-check, or a max-attempts dead-letter — that
//     names a specific durable row and that callers and this package's own
//     tests match with errors.Is/errors.As. A row-attributed entry ALWAYS
//     outranks an unattributed one, regardless of insertion order.
//   - Tier 2, UNATTRIBUTED entries (recordUnattributed's synthetic keys):
//     these carry a general, call-scoped reason this call stopped — ctx.Err()
//     from any of the three ctx-death exits, a failed lease attempt, a
//     pump-stopping decline, or a generic "still outstanding"/
//     "outstanding check unconfirmed" from the terminal emptiness check.
//     Those explain why the LOOP ended, not what happened to a row, so they
//     are only ever the representative when no row-attributed failure
//     survives.
//
// Within a tier, the FRESHEST surviving entry wins (unchanged from the
// pre-#624 rule): walk the order slice backwards, skipping entries whose key
// recordSuccess has since deleted. Deterministic despite Go's randomized map
// iteration, because it walks the parallel order slice rather than ranging
// over the map directly.
//
// Why the tier rule exists: before #624 the rule was purely freshest-wins,
// which let a GENERAL cause recorded later — most typically ctx.Err() folded
// in by verdictOnCtxDone or the execSem-wait exit, but equally a DB lease
// error or the outstanding-entries check's result — MASK a more specific,
// row-attributed failure already on record from an earlier iteration of the
// same loop. The verdict's DrainFailed/DrainComplete classification was
// never wrong (both tiers count toward hasFailures); what was wrong was the
// REPORTED CAUSE, which is exactly what a caller reads to decide what to do
// about the durable row. Substituting the general cause at the call sites
// was considered and rejected by the orchestrator in an earlier round — the
// fix belongs here, inside the ledger, so every exit path inherits it.
func (l *rowLedger) mostRecentFailure() error {
	var unattributedFallback error
	for i := len(l.order) - 1; i >= 0; i-- {
		key := l.order[i]
		err, ok := l.failed[key]
		if !ok {
			// Deleted by a later recordSuccess for this exact key -- dead
			// weight in the order slice, skip it.
			continue
		}
		if isUnattributedFailureKey(key) {
			// Tier 2: remember only the freshest one as a fallback; a
			// row-attributed entry found earlier in the walk (i.e.
			// inserted before it) still outranks it.
			if unattributedFallback == nil {
				unattributedFallback = err
			}
			continue
		}
		// Tier 1: a surviving row-attributed failure always wins.
		return err
	}
	return unattributedFallback
}

// verdictOnCtxDone is the ONLY sanctioned way for DrainSessionNow to compute
// a verdict at the moment it discovers ITS OWN ctx has ended — task #607,
// P0, the sixth review-cycle fix to this same contract. Five prior task
// reviews (#575, #578, #588, #592, and #592's own nil-safety follow-up
// #593) each closed one specific way this call's return value could
// misrepresent outstanding work as a clean success; every one of those
// fixes taught the SAME lesson in a different shape: a verdict computed
// without first recording WHY the loop is stopping silently inherits
// whatever the ledger already happened to hold, which is correct only by
// accident.
//
// This function makes that class of mistake unrepresentable at its two
// call sites (both immediately below the top-of-loop `if ctx.Err() != nil`
// check and the admission-wait `case <-ctx.Done()`) rather than merely
// checked by convention: unlike calling l.verdict(...) directly, there is
// no path through this function that reaches a verdict without first
// unconditionally folding ctx.Err() into the ledger as this call's own
// reason for stopping. Concretely, this closes the exact defect task #607
// reproduced — one row already recordSuccess'd, ctx dies before the next
// row is even looked at, and the bare `ledger.verdict(false)` these two
// sites used to call directly saw anyExecuted=true / failed empty and
// returned (DrainComplete, nil): a caller-facing "the continuation fully
// completed" for a call that in fact stopped because ITS OWN deadline (or
// caller-cancelled ctx) fired, with whatever else was pending never even
// looked at.
//
// This mirrors — deliberately — the THIRD ctx-death exit in this same
// loop (the execSem-wait `case <-ctx.Done()` a few dozen lines below
// admission-wait's), which already called `ledger.recordUnattributed(ctx.Err())`
// before its own verdict() call and was therefore never part of any of the
// five prior defects. The two sites this function replaces were the ONLY
// two of DrainSessionNow's five early-exit branches that did not already
// follow that pattern; this factors the pattern out so the two callers
// cannot drift from the third's (or each other's) behavior again by a
// future edit to just one of them.
//
// contended is always false here: both call sites reach this function
// because CTX ended, not because a genuinely different live owner made the
// session busy/contended (that is the separate, already-safe stopNow path,
// which passes contended=true directly to l.verdict and is not routed
// through this function — see DrainSessionNow's stopNow branches).
//
// GATED on l.anyExecuted, NOT unconditional (coordinator review of this
// task's first pass caught this): recordUnattributed(ctx.Err()) must only
// run when THIS call has already executed (or observed) at least one row.
// A ctx that dies before ANYTHING ran is the pre-existing, unaffected
// DrainNoWork contract every caller already depends on -- see rowLedger's
// own verdict() doc, which deliberately leaves that case alone even when
// contended is true. Recording unconditionally (this function's own first
// version) made verdict(false) return DrainFailed instead of DrainNoWork
// for that case: anyExecuted became true and a failed-map entry appeared
// where neither existed a moment before, so a call that genuinely did
// NOTHING started reporting "a row failed" -- overstating failure in
// exactly the mirror-image way the five prior task reviews overstated
// success. app_run.go's drainOutcomeError reads DrainNoWork as "let the
// ORIGINAL cancellation stand" and DrainFailed as "a row genuinely ran and
// did not resolve cleanly"; those are different claims about what
// happened, even though both currently exit non-zero, and callers other
// than today's app_run.go may reasonably rely on the distinction (that is
// the whole reason DrainResult has four values instead of a boolean).
func (l *rowLedger) verdictOnCtxDone(ctx context.Context) (DrainResult, error) {
	if l.anyExecuted {
		l.recordUnattributed(ctx.Err())
	}
	result, verdictErr := l.verdict(false)
	if result == DrainNoWork {
		// Reachable, and this IS its normal mode now: nothing had executed
		// in this call by the time ctx died (the anyExecuted guard above
		// left the ledger untouched), so verdict(false) takes its own
		// DrainNoWork branch and returns (DrainNoWork, nil) -- reinstated
		// here as ctx.Err() to preserve the historical "report ctx.Err()
		// directly when nothing has happened yet" behavior the two former
		// call sites both had: a caller cancelling THIS call's own ctx
		// before anything ran still needs to see that as an error, not
		// silence.
		return DrainNoWork, ctx.Err()
	}
	return result, verdictErr
}

// DrainSessionNow synchronously executes every currently-pending run-queue
// entry for sessionID, blocking the caller until the session's durable
// queue is empty and no execution of it is in flight in this pump instance
// -- or until ctx is done.
//
// It exists so a short-lived process (rush run) can finish a durable
// continuation in the SAME process instead of leaving it for some future
// invocation's background tick to eventually pick up (task #421/P0-1): a
// cross-process interrupt landing on a busy session cancels the in-flight
// generation and durably enqueues its replacement (handleInterruptTick,
// mailbox.go's FromDurableQueue guard), deliberately WITHOUT a live
// mb.replacement handoff -- the durable row is the only remaining owner.
// Without something calling this, that row sits pending until the
// background pump's next tick (RunQueuePumpInterval, 3s in production)
// happens to fire before the process exits -- a race the process routinely
// loses, since RunNonInteractive's own completion path runs in
// milliseconds after the cancellation.
//
// Returns a DrainResult other than DrainNoWork only if a continuation
// actually EXECUTED -- leased and run to completion by this call, or (see
// below) run by another local execution this call waited on and whose
// exact terminal outcome it observed. Leasing a row is not executing it,
// and this distinction is the whole of P0-1 in the 2026-08-18
// release-readiness review: drained used to be set the moment
// LeaseRunQueueEntry returned a row, before any execution was attempted.
//
// The wait-and-observe half was, until task #575 of the 2026-08-19
// release-readiness review, the wider and still-open half of the same
// defect: on losing the admission race to a same-pump background worker,
// this function used to set drained=true and poll admission until it
// cleared, WITHOUT learning what that worker's execution actually produced.
// "Admission cleared" was silently read as "a continuation completed here",
// which is true for only one of (at least) five real outcomes -- the row
// returned to pending by ErrCallQueuedNotExecuted/SessionLockBusyError, a
// terminal AlreadyAttempted failure, a failed Ack leaving the row leased, a
// lost lease handed to a new owner, or a genuine committed success -- and
// RunNonInteractive (internal/app/app_run.go) converts a fully-clean result
// directly into exit code 0 with a success envelope. admitSession now
// publishes an admissionEntry carrying a done channel and the admitted
// execution's actual executeEntrySync outcome (see run_queue_admission.go),
// and this function waits for that specific outcome and classifies it
// through EXACTLY the same classifyBackgroundOutcome helper its own
// executed-here branch uses, so the two branches cannot disagree about what
// a given error means.
//
// DrainNoWork, err=nil means nothing ran and nothing failed; callers must
// NOT treat that as having recovered anything (a plain user-initiated
// cancel/timeout with no durable continuation looks identical to "nothing
// to drain", and both must leave the caller's original outcome standing).
//
// DrainComplete, err=nil is the ONLY pairing a caller may read as "the
// continuation fully completed" and use to replace an original cancellation
// with success. Task #588/P0-2 of the 2026-08-19 release-readiness follow-up
// review found a "merely PARTIAL drain" was ALSO producing this pairing
// (now DrainPartial, see ErrDrainIncomplete's own doc), and task #592/P0-1
// of that review's own second follow-up found a DIFFERENT row's clean
// commit could mask an EARLIER row's terminal failure, Ack failure, or lease
// loss in the SAME call (now DrainFailed -- see rowLedger's own doc for the
// same-row-only identity rule that closes this). With stacked durable rows
// this is not hypothetical: an OS session lock or in-process mailbox owner
// can legitimately change hands, or a row can resolve ambiguously, BETWEEN
// two rows processed by the SAME DrainSessionNow call.
//
// Race against the background tick, WITHIN THIS PUMP INSTANCE: LeaseRunQueueEntry
// is atomic at the DB level, so two callers racing for the same row can
// never both execute it -- but if this pump instance's own background tick
// wins the race, THIS call's own lease attempt simply finds nothing
// pending, even though the row is genuinely being executed right now by a
// goroutine this call didn't start. Silently returning "nothing to drain"
// in that case would reproduce the exact bug this function exists to
// close, just via a race instead of a certainty. The fix: check admission
// for this session before concluding there is nothing left to wait for. If
// busy, wait for that specific execution's admissionEntry.done (bounded by
// ctx), then process its published outcome through
// classifyBackgroundOutcome -- the same code path this function's own
// leasing branch uses for an execution it ran itself.
//
// admitSession's in-process gate does NOT close the equivalent race against
// a DIFFERENT process, or a different RunQueuePump instance in this same
// process -- a prior version of this comment claimed otherwise, which was
// simply wrong. p.inFlight is an in-memory map scoped to one *RunQueuePump
// value; it has no visibility into any lease held by a different pump
// instance's leased_by. If a foreign owner leases a row for sessionID
// between this call's own lease attempts, GetOldestPendingRunQueueEntryForSession
// (status = 'pending' only) simply cannot see it, admitSession has nothing
// to refuse this call on (no in-process admissionEntry exists for a
// foreign lease), and the bottom-of-loop "nothing pending" branch used to
// fall straight through to ledger.verdict(false) -- reporting DrainComplete
// while that row sat durable, leased, and unresolved (task #610, P0: the
// seventh form of this same contract defect, and the first reachable with
// a live, non-cancelled ctx rather than requiring ctx cancellation to
// trigger). The fix is the explicit HasOutstandingRunQueueEntriesForSession
// check immediately before this function's own terminal
// ledger.verdict(false) call, at the bottom of the loop below -- it checks
// BOTH 'pending' and 'leased' for the session, by ANY owner, not just this
// call's own admission map or its own previously-leased rows.
//
// Deliberately does NOT replicate processEntry's RunQueueMaxAttempts
// pre-check, busyBackoffUntil dedup, or admitMu/stopping shutdown gate --
// those exist for the long-running, many-tick background scenario. A
// synchronous drain bounded by the caller's own ctx (rush run's --timeout)
// does not need them: a genuinely stuck or poison entry hits ctx's
// deadline (or, for attempts, the loop below still honors
// RunQueueMaxAttempts directly so a truly poison entry terminal-fails
// instead of being retried forever inside one call) and this call returns
// with that error rather than looping unboundedly.
//
// CONTRACT DECISION (task #578, P2-1 of the 2026-08-19 release-readiness
// review; row-identity enforcement added by task #592/P0-1 of that review's
// second follow-up): once a retry at the same logical row later resolves,
// that later resolution supersedes an earlier retryable failure AT THE SAME
// ROW -- option (a) from the review, not (b). The loop keeps retrying rather
// than stopping at the first error (needed for the ordinary "nack, then
// succeed on the next tick" case every existing caller already depends on),
// but a row's own failure must never be cleared by a DIFFERENT row's
// success. rowLedger (above) is what enforces this: recordFailure/
// recordSuccess are both keyed by row ID, and the ONLY way an entry leaves
// the ledger's failed set is a later recordSuccess for that EXACT ID -- see
// the two call sites below (the observed-admission branch and the
// executed-here branch), and the lastRowID re-check just before the final
// "nothing pending" return, which closes the cross-process variant: a
// retryable local failure Nacks its row back to pending BEFORE this
// function ever records anything for it in the ledger's terminal read, and
// a genuinely different process's pump instance can lease that same row
// before this loop's own next attempt -- this call's in-memory admission map
// cannot see that, so without the re-check it would report its own stale
// failure for a row whose actual fate is now unknown to it. Option (b) --
// stop at the first error -- was rejected: it would surface transient,
// already-superseded failures to RunNonInteractive's caller even when the
// very next tick (in this process or another) cleanly finishes the same
// work, which is worse for the dominant case (a single retryable hiccup)
// than the bounded re-check below.
//
// "Supersedes" does NOT mean "clears to nil": the re-check NEVER clears a
// row's failure entry to a confirmed success, because this schema gives it
// no way to positively confirm a genuine commit happened -- AckRunQueueEntry
// and TerminalFailRunQueueEntry are both a plain DELETE ... RETURNING id
// query (see sql/run_queue.sql), so a row that is gone after a different
// process touched it is exactly as ambiguous, after the fact, as one still
// leased by a different process: it might be a real success, or it might be
// accepted work permanently lost to a terminal failure. Both outcomes
// record into the ledger as failures (ErrRowOutcomeUnconfirmed for "gone",
// errLeaseLost's own meaning for "still leased elsewhere") -- see the
// re-check's own comment below for why "gone" cannot mean "succeeded" in
// this schema. RunNonInteractive (internal/app/app_run.go) converts
// DrainComplete directly into exit code 0 with a success envelope; treating
// an unconfirmable outcome as one reopens exactly the false-success class
// task #575 (commit 638bc777) closed, just through this re-check path
// instead of the admission-wait path #575 fixed. The literal "nack, then
// the SAME process's own retry succeeds" case this task names never
// reaches the re-check at all -- it clears the ledger's entry for that exact
// row via recordSuccess above, backed by a real, locally-observed
// executeEntrySync success.
