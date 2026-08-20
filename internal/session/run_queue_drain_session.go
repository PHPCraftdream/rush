// Synchronous drain: DrainSessionNow lets a short-lived process finish
// pending durable entries for a session in-process instead of leaving them
// for some future background tick, plus its lock-busy error helper.

package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
//   - DrainNoWork: nothing executed in this call, and nothing failed. This
//     is the ONLY value under which a caller may leave an original
//     cancellation/error standing without inspecting err further — it means
//     "there was nothing for this call to do", not "everything succeeded".
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
	// DrainNoWork means nothing executed and nothing failed — the
	// pre-existing, still-correct "nothing to drain" contract every caller
	// already depends on. err is always nil when result is DrainNoWork.
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
	key := fmt.Sprintf("__unattributed_%d", l.unattributedSeq)
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

// mostRecentFailure returns the failure ledger's most-recently-inserted
// SURVIVING error — i.e. the last key in insertion order that is still
// present in the failed map (recordSuccess may have deleted more recent
// entries for OTHER rows without touching this one). Deterministic despite
// Go's randomized map iteration, because it walks the parallel order slice
// rather than ranging over the map directly.
func (l *rowLedger) mostRecentFailure() error {
	for i := len(l.order) - 1; i >= 0; i-- {
		if err, ok := l.failed[l.order[i]]; ok {
			return err
		}
	}
	return nil
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
// It exists so a short-lived process (crush run) can finish a durable
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
// synchronous drain bounded by the caller's own ctx (crush run's --timeout)
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
func (p *RunQueuePump) DrainSessionNow(ctx context.Context, sessionID string) (DrainResult, error) {
	ledger := newRowLedger()

	// lastRowID names the run-queue row (by ID) that the ledger's most
	// recent LOCAL failure describes, so the 'nothing pending' branch below
	// can re-check that SPECIFIC row's current state before handing the
	// ledger's verdict back to the caller as the final answer -- see the
	// contract decision above. Set only for an ordinary retryable failure
	// (the row Nacked back to pending); cleared on a clean success, a
	// terminal resolution, or an outcome read from a wait on someone else's
	// admissionEntry (already fully resolved, and not a row this call ever
	// leased itself).
	var lastRowID string
	for {
		if ctx.Err() != nil {
			// task #607/P0: route through verdictOnCtxDone, NOT a bare
			// ledger.verdict(false) — see that function's own doc. A bare
			// verdict() call here trusts the ledger's PRE-EXISTING state
			// (whatever an earlier iteration recorded) to already explain
			// why this call is stopping, but "ctx died" is itself a new,
			// this-call-scoped reason the ledger has not been told about
			// yet: if an earlier row already recordSuccess'd and nothing
			// else has failed, the bare call used to return (DrainComplete,
			// nil) — a false "fully completed" for a call that in fact
			// stopped here because its OWN deadline fired, with the row
			// that triggered this loop iteration never even looked at.
			return ledger.verdictOnCtxDone(ctx)
		}

		// Reserve the session through the SAME atomic gate the background
		// tick uses, and do it BEFORE leasing (P1-1 of the 2026-08-18
		// release-readiness review). This call used to lease first and then
		// assign the shared marker unconditionally, which let a drain start
		// row B for a session a background worker was already executing row
		// A for -- two executions, one boolean, and whichever finished first
		// cleared it. See admitSession.
		releaseSession, otherEntry, admitted := p.admitSession(sessionID)
		if !admitted {
			// Something in THIS pump instance is already executing for this
			// session -- the background tick, having won the lease race, or
			// (in principle) a second concurrent drain call. Wait for its
			// SPECIFIC outcome via the admissionEntry admitSession's refusal
			// just handed back, because that outcome matters exactly as much
			// as this call's own would -- its messages reach the caller
			// through the same subscription, and its terminal fate
			// (committed, terminal-failed, ack-failed, lease-lost, or
			// genuinely just busy) determines whether this call may report
			// a complete drain at all.
			//
			// otherEntry comes from admitSession's OWN refusal branch, read
			// under the SAME lock as the refusal itself -- not from a
			// separate follow-up lookup. A separate lookup here used to be
			// exactly how task #587/P0-1's ABA race got in: the entry that
			// caused this refusal could finish and release, a completely
			// different execution for the same sessionID could be admitted
			// in its place, and a follow-up lookup would find and wait on
			// THAT replacement instead -- silently misattributing its outcome
			// to the execution this call actually lost the race to. Because
			// otherEntry is returned atomically with the refusal, that
			// window no longer exists: this is provably the same entry the
			// refusal observed, full stop. (Nothing needs to be done for a
			// "not found" case any more, either -- the map lookup and the
			// refusal decision are now the same read.)
			if p.cfg.TestAfterAdmissionRefusal != nil {
				p.cfg.TestAfterAdmissionRefusal(sessionID)
			}

			select {
			case <-ctx.Done():
				// task #607/P0: see the top-of-loop ctx.Err() branch's
				// comment above — same defect, same fix. This call is
				// giving up on waiting for otherEntry's outcome because ITS
				// OWN ctx ended, not because otherEntry resolved; that fact
				// must be folded into the ledger before computing the
				// verdict, or an earlier row's clean recordSuccess makes
				// this return (DrainComplete, nil) while otherEntry's
				// outcome — and whatever else might be pending — was never
				// learned.
				return ledger.verdictOnCtxDone(ctx)
			case <-otherEntry.done:
			}

			// otherEntry.outcome is now safe to read: done's close
			// happens-after the write (see admissionEntry's own doc).
			// Classify it through the exact same helper the executed-here
			// branch below uses, and apply it with the exact same
			// loop-vs-return shape: only the "busy, nothing ran" outcome
			// returns immediately (see classifyBackgroundOutcome's own doc
			// for why every other outcome instead loops back to check for
			// more pending work).
			outcomeDrained, outcomeRowID, outcomeErr, stopNow := classifyBackgroundOutcome(otherEntry.outcome)
			if outcomeDrained {
				// This outcome came from admissionEntry.done, which only
				// closes AFTER the other execution's own Ack/Nack/
				// TerminalFail write has already landed (see
				// admissionEntry's doc) -- it is a fully resolved, known
				// outcome.
				//
				// task #613/F4: when the OTHER execution's outcome carries a
				// row ID (outcomeExecuted or outcomeTerminalDeleted --
				// always true once execution genuinely happened, since
				// admitSession's admitted caller always knows which row it
				// leased), record it under THAT SAME row ID via the
				// ordinary recordFailure/recordSuccess path -- exactly like
				// the executed-here branch below -- so a LATER resolution of
				// the SAME logical row (whether observed via a second wait,
				// or locally leased and executed by THIS call's own retry)
				// can supersede it via recordSuccess(rowID). The pre-#613
				// code always fell back to a synthetic
				// __unattributed_N key here regardless of whether the row ID
				// was known, which made an observed retryable failure
				// permanently unclearable even by a same-row success this
				// call went on to execute itself -- see recordUnattributed's
				// own doc for why a synthetic key can NEVER be cleared, and
				// this file's own header comment for the false-DrainFailed
				// sequence this produced (F4).
				//
				// A row ID is only ever absent here for outcomeNoRowTouched
				// (never reaches this branch -- outcomeDrained is false for
				// it) or outcomeBusy (also never reaches this branch --
				// handled entirely by stopNow below), so outcomeRowID is
				// unconditionally non-empty whenever outcomeDrained is true;
				// the empty-ID fallback exists only as defense in depth
				// against a future outcomeKind that reaches this branch
				// without one.
				switch {
				case outcomeRowID != "":
					if outcomeErr != nil {
						ledger.recordFailure(outcomeRowID, outcomeErr)
					} else {
						ledger.recordSuccess(outcomeRowID)
					}
				case outcomeErr != nil:
					ledger.recordUnattributed(outcomeErr)
				default:
					// A genuine observed success has no row identity to
					// clear either -- but there is nothing in the ledger
					// FOR this wait's own row to clear (unattributed
					// failures are per-occurrence, not per logical row, by
					// construction -- see recordUnattributed's doc), so
					// this is just "record that something executed".
					ledger.recordSuccess("")
				}
				lastRowID = "" // resolved via the wait, not this call's own lease
			}
			if stopNow {
				// P0-2/task #588 (row-identity enforced by task #592/P0-1):
				// the ledger's accumulated state must NOT be silently
				// discarded here. If an EARLIER iteration (this call's own
				// executed-here branch, or an earlier wait) already recorded
				// a retryable/terminal/Ack-failure/unconfirmed outcome for
				// ANY row, that outcome is still the truth about this
				// session's queue and must reach the caller as DrainFailed
				// -- never silently replaced by a bare success that looks
				// clean. And even when every row so far has resolved
				// cleanly, if at least one row DID execute -- meaning this
				// session was genuinely, partially drained -- reporting a
				// full DrainComplete is exactly the false-success class
				// task #588 exists to close: this row's busy outcome means
				// the queue was NOT fully drained, only PARTIALLY, and the
				// caller must not read that as "the continuation is
				// complete". See ErrDrainIncomplete's own doc for the full
				// contract and why DrainNoWork is deliberately left alone
				// (unaffected pre-existing "nothing happened" case).
				return ledger.verdict(true)
			}
			continue
		}

		leased, leaseErr := p.cfg.Sessions.LeaseRunQueueEntry(ctx, sessionID, p.cfg.PumpInstanceID, p.leaseTTL())
		if leaseErr != nil {
			// The lease ATTEMPT failed (a DB error) -- executeEntrySync was
			// never reached, so no durable row was touched. Publish
			// noRowTouched(), not an outcome carrying leaseErr itself:
			// leaseErr is non-nil, but classifyBackgroundOutcome does not
			// treat "err is non-nil" as the signal for "nothing happened" --
			// only outcomeBusy and outcomeNoRowTouched map to
			// stopNow/no-drain. Publishing leaseErr directly (as an executed
			// outcome) would fall through to classifyBackgroundOutcome's
			// default branch and tell a waiting DrainSessionNow that an
			// execution genuinely happened here, which is false -- this
			// call's own return value (leaseErr, non-nil) is what correctly
			// reports the failure to THIS call's own caller; the outcome
			// published to a DIFFERENT, waiting caller must instead say
			// "nothing ran, retry admission".
			releaseSession(noRowTouched())
			result, _ := ledger.verdict(false)
			if result == DrainNoWork {
				return DrainNoWork, leaseErr
			}
			// A prior row already executed in this same call; the lease
			// failure that stopped this iteration is itself a new,
			// unattributed failure for THIS call's own return (not a row
			// a waiter could ever retry into), so it must not silently
			// vanish behind an earlier row's clean ledger state either.
			ledger.recordUnattributed(leaseErr)
			return ledger.verdict(false)
		}

		if leased != nil {
			// NOTE: this row is deliberately NOT recorded as executed yet.
			// Holding a lease is not the same as having executed anything --
			// see the P0-1 note in this function's doc. It is recorded
			// below, once executeEntrySync has actually run a turn.

			// Mirrors processEntry's own attempts-exhausted check (see
			// there) -- this call bypassed that check by leasing directly
			// instead of scanning pending entries first, so it must be
			// re-applied here or a poison entry that always fails would
			// retry inside this loop until ctx's deadline instead of
			// terminal-failing at RunQueueMaxAttempts like every other
			// path does.
			if leased.Attempts >= RunQueueMaxAttempts && !leased.TerminalFailure {
				termCtx, termCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
				termErr := p.cfg.Sessions.TerminalFailRunQueueEntry(termCtx, leased.ID, p.cfg.PumpInstanceID)
				termCancel()
				if termErr != nil {
					slog.Error("run_queue_pump: DrainSessionNow terminal fail failed", "id", leased.ID, "session_id", sessionID, "err", termErr, "instance_id", p.cfg.PumpInstanceID)
				}
				ledger.recordFailure(leased.ID, fmt.Errorf("run queue entry %q exceeded max attempts", leased.ID))
				// task #613/F3: this row was terminal-failed (DELETED) by a
				// DIRECT write here, not by executeEntrySync -- mirrors
				// processEntry's own attempts-exhausted branch (see its own
				// comment). THIS call's own ledger already carries the
				// max-attempts failure for its own caller (recordFailure
				// above), so this call's own return value is correct
				// regardless. But a DIFFERENT, waiting caller needs to learn
				// the SAME thing: publishing noRowTouched() here (the pre-#613
				// behavior) told a waiter "nothing ran, retry admission and
				// look for other pending work" -- indistinguishable from a
				// harmless early return, even though the row is now
				// permanently gone. Publish a row-scoped
				// terminalDeletedOutcome instead, so a waiter records the
				// SAME failure this call just recorded, under the SAME row
				// ID, instead of silently losing it.
				releaseSession(terminalDeletedOutcome(leased.ID, termErr))
				continue
			}

			select {
			case p.execSem <- struct{}{}:
			case <-ctx.Done():
				// Release the lease we just took rather than leaving it
				// leased with nobody executing it. A later tick (this
				// process or another) recovers it via the ordinary
				// lease-expiry path regardless, but releasing promptly
				// avoids waiting out a full TTL for no reason.
				//
				// executeEntrySync was never reached -- this call's own ctx
				// ended before an execution slot was even available -- so a
				// waiter must be told noRowTouched(), not an outcome carrying
				// ctx.Err() itself: like the leaseErr case above, a non-nil
				// error here is not the signal classifyBackgroundOutcome
				// looks for.
				releaseSession(noRowTouched())
				nackCtx, nackCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
				if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(nackCtx, leased.ID, p.cfg.PumpInstanceID, "run_queue_pump: DrainSessionNow's ctx ended before an execution slot was available"); nackErr != nil {
					slog.Error("run_queue_pump: DrainSessionNow release-on-ctx-done nack failed", "id", leased.ID, "session_id", sessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
				}
				nackCancel()
				result, _ := ledger.verdict(false)
				if result == DrainNoWork {
					return DrainNoWork, ctx.Err()
				}
				ledger.recordUnattributed(ctx.Err())
				return ledger.verdict(false)
			}

			// Register with workerWg for the execution, so Stop() waits for
			// this drain exactly as it waits for a background worker.
			//
			// Without it (P0-2 of the 2026-08-18 release-readiness review)
			// Stop() could see an idle pump while this call was mid-turn,
			// and App.Shutdown would then close the database underneath a
			// live execution. The Add happens under admitMu, matching the
			// ordering processEntry relies on: Stop sets 'stopping' under
			// that same mutex before calling Wait, so an Add can never race
			// a Wait that has already begun.
			//
			// If a stop is already underway the lease is released instead of
			// executed -- starting a turn that Stop is known not to wait for
			// is how the shutdown hole reopens.
			p.admitMu.Lock()
			stopping := p.stopping
			if !stopping {
				p.workerWg.Add(1)
			}
			p.admitMu.Unlock()

			if stopping {
				releaseSession(busyOutcome(ErrCallQueuedNotExecuted))
				<-p.execSem
				nackCtx, nackCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
				if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(nackCtx, leased.ID, p.cfg.PumpInstanceID, "run_queue_pump: DrainSessionNow declined to start because the pump is stopping"); nackErr != nil {
					slog.Error("run_queue_pump: DrainSessionNow release-on-stopping nack failed", "id", leased.ID, "session_id", sessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
				}
				nackCancel()
				result, _ := ledger.verdict(false)
				if result == DrainNoWork {
					return DrainNoWork, ErrCallQueuedNotExecuted
				}
				ledger.recordUnattributed(ErrCallQueuedNotExecuted)
				return ledger.verdict(false)
			}

			execErr := p.executeEntrySync(ctx, leased)
			p.workerWg.Done()

			<-p.execSem
			releaseSession(executedOutcome(leased.ID, execErr))

			// Classify through the same helper the wait branch above uses.
			// Only the "busy, nothing ran" outcome returns immediately --
			// every other outcome (success, terminal failure, failed Ack,
			// lost lease, ordinary retryable failure) means an execution
			// genuinely happened, so the ledger is updated and the loop
			// continues to check for more pending work (e.g. a second
			// stacked interrupt), exactly as the pre-existing "turn actually
			// ran" branch below already did before this outcome could also
			// arrive via a wait.
			outcomeDrained, _, outcomeErr, stopNow := classifyBackgroundOutcome(executedOutcome(leased.ID, execErr))
			if outcomeDrained {
				if outcomeErr == nil {
					// A clean success: THIS row's ID is known (leased.ID),
					// so record it against that exact ID -- clearing any
					// earlier failure THIS SAME ROW had accumulated (the
					// literal same-row-retry-succeeds case), and touching
					// nothing recorded for any OTHER row.
					ledger.recordSuccess(leased.ID)
					lastRowID = ""
				} else {
					// Every failure outcome here is recorded against
					// leased.ID: a terminal failure (AlreadyAttempted) or a
					// lost lease also leave nothing for a different pump
					// instance to race in on the same row, so a later
					// iteration of THIS loop will never revisit leased.ID
					// again either way -- but recording it by ID rather
					// than as unattributed keeps the same-row-supersede
					// path available for the one case that DOES revisit
					// the row: an ordinary retryable failure, Nacked back
					// to pending, which ends up leased again by this exact
					// loop on its very next iteration.
					ledger.recordFailure(leased.ID, outcomeErr)
					if isOrdinaryRetryableOutcome(execErr) {
						// Track the row's ID for the bottom-of-loop
						// cross-process re-check ONLY for the retryable
						// case: it is the one whose 'nothing pending next
						// iteration' resolution is genuinely ambiguous
						// between 'fully resolved' (this call's own next
						// iteration will re-lease and retry it) and
						// 'someone else has it now' (a different process
						// raced this call's Nack), and the only case the
						// re-check below needs to run for.
						lastRowID = leased.ID
					} else {
						lastRowID = ""
					}
				}
			}
			if stopNow {
				// A genuinely different live owner has this session right
				// now -- not this call, not the background tick (see those
				// errors' own docs on executeEntrySync). NOTHING RAN here:
				// the call was appended to that owner's mailbox, or the OS
				// session lock was held, and the row was released without an
				// attempt penalty for someone else to pick up later.
				//
				// So the ledger's own "did anything execute" state stays as
				// it was -- DrainNoWork unless an EARLIER iteration of this
				// loop genuinely executed something.
				//
				// Waiting for a stranger's turn to finish is not this call's
				// job either: stop, and report the truth about this
				// session's queue instead of a bare success.
				//
				// P0-2/task #588 (row-identity enforced by task #592/P0-1):
				// the ledger must NOT be silently discarded here, for
				// exactly the same reason as the wait branch above -- see
				// ErrDrainIncomplete's own doc. An earlier iteration in THIS
				// SAME loop may have already recorded a real failure for
				// SOME row (reported as DrainFailed, regardless of what any
				// other row did), or may have committed every row it
				// touched cleanly (reported as DrainPartial: this row's busy
				// outcome means the queue was only PARTIALLY drained, so a
				// full DrainComplete would be wrong). DrainNoWork (nothing
				// has run in this call at all yet) is left alone -- the
				// pre-existing, still-correct "nothing happened here"
				// contract every caller depends on.
				return ledger.verdict(true)
			}
			continue // more may be pending (e.g. a second stacked interrupt) -- loop and check again
		}

		// Nothing pending, and nothing else can be executing for this
		// session either -- this call currently holds its only admission.
		//
		// The "is someone else busy with it?" check used to live HERE,
		// reached only after a lease attempt came back empty, which is
		// exactly what made the old ordering racy: by then this call had
		// already leased and marked. It now runs before leasing, at the top
		// of the loop.
		//
		// noRowTouched(), not a bare nil: nil is executeEntrySync's own
		// return value for a clean commit, and a nil outcome published here
		// would be indistinguishable from one to a second, concurrent
		// DrainSessionNow call that lost the admission race against THIS
		// call and is waiting on it -- telling that waiter a continuation
		// completed when this call in fact found nothing pending at all
		// (the coordinator's concrete false-success scenario for task #575:
		// a second process leases the row first, this call's own leased ==
		// nil branch below is what runs, and the OLD nil here is exactly
		// what a waiting drain would misread as success).
		releaseSession(noRowTouched())

		// P2-1 fix: the ledger's terminal read may still describe a row
		// this call Nacked on an EARLIER iteration and no longer has any
		// claim over -- the Nack landed (inside executeEntrySync) before
		// this function ever recorded that row's outcome, so the row was
		// already back in 'pending' the moment releaseSession ran above,
		// open to a genuinely different process's pump instance leasing it
		// before this loop's own next iteration got back around to it
		// (in-memory admission -- p.inFlight -- only guards against races
		// within THIS process; it cannot see another process's lease).
		// Reaching this branch with lastRowID set means exactly that
		// happened: LeaseRunQueueEntry just found nothing pending for a row
		// we know we returned to pending ourselves.
		//
		// Re-check that SPECIFIC row before trusting the ledger's failure
		// entry for it as the final answer -- but this re-check can NEVER
		// clear that entry to a confirmed success. That is deliberate, not
		// an oversight: an earlier version of this fix treated "the row is
		// gone" (GetRunQueueEntry returns nil) as proof of a genuine commit
		// and cleared it, but AckRunQueueEntry and TerminalFailRunQueueEntry
		// are BOTH a plain DELETE FROM session_run_queue ... RETURNING id
		// query (see sql/run_queue.sql) -- there is no terminal_failure flag
		// left behind to distinguish them by (the terminal_failure COLUMN
		// exists in the schema, but no query ever sets it to 1 on a
		// surviving row; TerminalFailRunQueueEntry deletes outright, exactly
		// like Ack, so RunQueueEntry.TerminalFailure is always false in
		// practice -- confirmed by reading every query in that file, not
		// assumed). A row gone after a DIFFERENT process touched it is
		// therefore EXACTLY as ambiguous as one still leased by a different
		// process: it might be a genuine commit, or it might be a permanent
		// terminal-fail for accepted work that will now never run.
		// app_run.go's RunNonInteractive reads DrainComplete as "a durable
		// continuation completed here" and converts it directly into exit
		// code 0 (see run_queue_pump.go's own DrainSessionNow doc and task
		// #575/commit 638bc777, which closed five different ways a bare nil
		// could misrepresent a non-success outcome as one) -- asserting
		// success on an outcome this call cannot actually confirm reopens
		// exactly that defect, just through this re-check path instead of
		// the admission-wait path #575 fixed, or the cross-row path #592
		// fixed. The literal same-process "nack, then retry succeeds" case
		// this task names does NOT go through this re-check at all: THAT
		// case re-leases the SAME row itself on the next loop iteration and
		// clears the ledger's entry for that exact row via recordSuccess
		// above, with a real executeEntrySync outcome behind it. This
		// re-check only covers the cross-process case, where a positive
		// success can never be confirmed from this schema -- so it never
		// fabricates one.
		//   - gone entirely (current == nil): ambiguous between "acked" and
		//     "terminal-failed by another process" -- ErrRowOutcomeUnconfirmed
		//     is deliberately the more conservative of the two (a false
		//     "still might fail" costs the operator a retry; a false
		//     "succeeded" costs them silently losing accepted work), so an
		//     unresolvable ambiguity is reported through it rather than
		//     guessing success.
		//   - still present and 'leased' (status alone, NOT compared against
		//     this call's own p.cfg.PumpInstanceID as leasedBy -- the code
		//     does not distinguish "someone else has it" from "this same
		//     pump instance's own background tick re-leased it"): outcome
		//     is genuinely UNKNOWN to THIS call either way, so it is
		//     reported through errLeaseLost's own "nothing further can be
		//     learned from this attempt" meaning regardless of which one it
		//     actually is -- the conservative answer is correct in both
		//     cases, so the missing identity check costs nothing today, but
		//     the label is "leased", not "leased by a different owner".
		//   - still present and 'pending' (a lookup race with the lease
		//     attempt just above, or the DB write hasn't settled from this
		//     call's own perspective): the ledger's entry for it still
		//     describes it accurately, since nobody else has touched it.
		//     Left as recorded.
		//   - the recheck read itself fails: fall back to returning the
		//     ledger's verdict as originally recorded rather than losing a
		//     real error to a transient lookup failure -- see below.
		if lastRowID != "" {
			recheckCtx, recheckCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
			current, recheckErr := p.cfg.Sessions.GetRunQueueEntry(recheckCtx, lastRowID)
			recheckCancel()
			switch {
			case recheckErr != nil:
				// Could not confirm either way -- degrade to the pre-fix
				// behavior (report what we last knew) rather than silently
				// dropping a real error over a transient lookup failure.
				slog.Warn("run_queue_pump: DrainSessionNow could not re-check a locally-nacked row's current state; reporting its last known local outcome", "id", lastRowID, "session_id", sessionID, "err", recheckErr, "instance_id", p.cfg.PumpInstanceID)
			case current == nil:
				// Ambiguous: acked (genuine success) or terminal-failed
				// (permanent loss) by a different process -- both delete
				// the row identically, and nothing else in this schema
				// distinguishes them. Report the conservative outcome,
				// still keyed by the SAME row ID so a hypothetical future
				// resolution for this exact ID (there is none reachable
				// here -- the row is gone) would still target the right
				// ledger entry.
				ledger.recordFailure(lastRowID, fmt.Errorf("%w (id=%s)", ErrRowOutcomeUnconfirmed, lastRowID))
			case current.Status == "leased":
				// A DIFFERENT owner holds it right now. Unknown, not
				// resolved -- report that, not a fabricated success.
				ledger.recordFailure(lastRowID, fmt.Errorf("%w (id=%s)", errLeaseLost, lastRowID))
			}
			// current != nil && current.Status == "pending": still ours to
			// account for, and nobody else has touched it -- leave the
			// ledger's entry for it as recorded.
		}

		// task #610/P0: the SEVENTH form of this same contract defect, and
		// the first reachable with a live, non-cancelled ctx. Everything
		// above this point re-checks a row THIS call itself touched
		// (lastRowID); it says nothing about a row this call never leased at
		// all -- e.g. a genuinely different process (or a different pump
		// instance in this same process, racing this call) that leased row B
		// for sessionID between this call's last successful row and this
		// exact iteration, and has not yet Ack'd, Nack'd, or terminal-failed
		// it. LeaseRunQueueEntry only ever looks at status = 'pending'
		// (see GetOldestPendingRunQueueEntryForSession in sql/run_queue.sql),
		// so a leased-by-someone-else row is invisible to it: this call's own
		// lease attempt above finds "nothing pending" and, without the check
		// below, falls straight through to ledger.verdict(false) -- which
		// resolves to (DrainComplete, nil) whenever the ledger otherwise has
		// no failures recorded, even though row B is durable, outstanding,
		// and its eventual outcome is completely unknown to this call.
		//
		// A reviewer reproduced this concretely: one row (A) genuinely
		// executed and committed in this call, a second row (B) for the same
		// session was leased by a different owner and never resolved, and
		// DrainSessionNow returned (DrainComplete, nil) while B still sat in
		// the DB with status=leased, attempts=0, and an expired lease --
		// `crush run` had already exited 0 over durable, unconfirmed work.
		//
		// The fix is a dedicated, explicit service call --
		// HasOutstandingRunQueueEntriesForSession -- rather than inferring
		// "queue empty" from GetOldestPendingRunQueueEntryForSession's
		// pending-only failure to find a row, precisely because that
		// inference is exactly the gap being closed: a query scoped to
		// 'pending' can never answer "is the queue empty" on its own, no
		// matter how its failure is read. The new query checks BOTH
		// 'pending' and 'leased' explicitly.
		//
		// An outstanding row here is reported as a FAILURE (never silently
		// ignored, and never upgraded to a fabricated success) -- mirrors the
		// lastRowID re-check's own "gone means ambiguous, not success" stance
		// a few lines above: this call cannot Ack, Nack, or terminal-fail a
		// row it does not hold the lease for, so DrainComplete must never be
		// returned while one exists. A prior row's clean recordSuccess in
		// THIS same call does not clear this: rowLedger's identity rule (see
		// its own doc) means B's outstanding presence can only ever ADD to
		// the verdict, never be subtracted by A's unrelated success.
		//
		// Deliberately keyed under a synthetic recordUnattributed entry, not
		// under B's own row ID via recordFailure: this call never leased B,
		// so it has no executeEntrySync-observed outcome to attach to B's
		// identity, and inventing one here would incorrectly imply this call
		// knows something about B's specific fate beyond "still outstanding"
		// -- recordUnattributed's per-occurrence key is exactly the shape for
		// "a failure this call observed but cannot tie to a row it leased
		// itself" (see its own doc), which is precisely this situation.
		//
		// An expired-but-not-yet-recovered lease counts as outstanding here,
		// same as a fresh one: CleanupExpiredLeases (the pump's periodic
		// maintenance sweep) has not yet run on it, so from THIS call's point
		// of view its outcome is still genuinely unknown -- it might resolve
		// cleanly on the next sweep, or it might not. Treating expiry as
		// equivalent to "gone" here would let this exact defect back in
		// through the expiry window: exit code correctness cannot depend on
		// timing a maintenance sweep that this call does not control and is
		// not obligated to wait for. If this check should ever be relaxed to
		// treat an expired lease as equivalent to "gone", that is a distinct,
		// deliberate contract change -- not a fold-in here.
		//
		// Gated on TWO conditions, both required, mirroring
		// verdictOnCtxDone's own anyExecuted gate a few dozen lines above
		// (that function's doc explains why the analogous mistake there --
		// recording unconditionally -- turned a genuine DrainNoWork into a
		// false DrainFailed; the same shape of mistake is possible here):
		//
		//   - ledger.anyExecuted: if NOTHING has executed (or been observed)
		//     in this call at all, this is the pre-existing, unaffected
		//     DrainNoWork contract every caller already depends on -- a
		//     session that was never touched by this call, where some
		//     OTHER, unrelated row happens to be outstanding, is not this
		//     call's business to report on any more than a busy/contended
		//     session is (see rowLedger.verdict's own top branch, which
		//     leaves DrainNoWork alone even when contended is true).
		//     Skipping the query entirely here also avoids a real defect
		//     this gate was found catching directly: a losing admission race
		//     whose winner has not yet resolved ANYTHING (e.g. a background
		//     processEntry parked mid-lease) leaves exactly such a row
		//     outstanding for the ENTIRE session, and querying unconditionally
		//     turned that pre-existing, correct DrainNoWork into a false
		//     DrainFailed the moment this check was added -- caught by this
		//     package's own TestProcessEntry_RacedLeaseNil_DoesNotFalselyDrainAWaiter.
		//   - !ledger.hasFailures(): if the ledger already has a surviving
		//     failure recorded, verdict(false) below is already going to
		//     return DrainFailed regardless of what this check finds
		//     (rowLedger.verdict checks len(failed) > 0 before it ever looks
		//     at contended), so querying here would only ever add a
		//     REDUNDANT, more-recently-inserted synthetic entry that could
		//     SHADOW an already-diagnosed, row-specific cause via
		//     mostRecentFailure's freshest-surviving-entry rule -- e.g. row
		//     A's specific ErrTurnCommitFailed/errLeaseLost/AlreadyAttempted
		//     failure, still sitting in the ledger under A's own ID
		//     precisely so a caller (and this package's own tests) can see
		//     it, getting replaced in the reported error by a generic
		//     "still outstanding" message that names no specific row and
		//     carries no specific cause. hasFailures() is the same
		//     len(failed) > 0 test verdict() itself performs, kept in sync
		//     with it deliberately (see hasFailures' own doc) rather than
		//     re-derived here.
		if ledger.anyExecuted && !ledger.hasFailures() {
			hasOutstanding, outstandingErr := p.cfg.Sessions.HasOutstandingRunQueueEntriesForSession(ctx, sessionID)
			switch {
			case outstandingErr != nil:
				// task #610's own review follow-up, P0 (the eighth form of
				// this contract's recurring defect): unlike the lastRowID
				// re-check a few dozen lines above, there is no "last known
				// local outcome" to degrade to here -- this call never
				// touched whatever row(s) this query would have found, so
				// it has no prior knowledge of its own to fall back on.
				// "The confirmation attempt itself failed" and "the queue
				// is confirmed empty" are different claims; silently
				// falling through to ledger.verdict(false) here would
				// report the latter while only the former is true --
				// reproducing task #610's own false-DrainComplete defect
				// through the confirmation query's failure path instead of
				// its result. SQLITE_BUSY here is not a hypothetical: it is
				// exactly the failure mode a genuinely different live
				// owner holding a row for this session, at this same
				// moment, would produce. Record it as a distinct,
				// unconfirmed-not-outstanding failure via
				// ErrOutstandingCheckUnconfirmed (wrapping sessionID and
				// the underlying DB error so a caller can recover the root
				// cause), never silently swallowed.
				slog.Warn("run_queue_pump: DrainSessionNow could not confirm the session's run queue is fully empty", "session_id", sessionID, "err", outstandingErr, "instance_id", p.cfg.PumpInstanceID)
				ledger.recordUnattributed(fmt.Errorf("%w (session=%s): %w", ErrOutstandingCheckUnconfirmed, sessionID, outstandingErr))
			case hasOutstanding:
				ledger.recordUnattributed(fmt.Errorf("%w (session=%s)", ErrOutstandingRunQueueEntry, sessionID))
			}
		}

		return ledger.verdict(false)
	}
}

// isOrdinaryRetryableOutcome reports whether execErr is the kind of failure
// that leaves its row back in 'pending' state via a plain NackRunQueueEntry
// (executeEntrySync's default failure branch) rather than one of the other
// outcomes (clean success, terminal AlreadyAttempted failure, a
// busy/queued/lock outcome handled by NackRunQueueEntryNoAttemptPenalty, or
// errLeaseLost/ErrTurnCommitFailed, which leave the row leased or otherwise
// don't reopen a plain 'pending' race the same way). Used by DrainSessionNow
// to decide whether a row's ID is worth re-checking once the loop later
// finds nothing pending for the session -- see the contract decision atop
// DrainSessionNow.
func isOrdinaryRetryableOutcome(execErr error) bool {
	if execErr == nil {
		return false
	}
	if errors.Is(execErr, errLeaseLost) || errors.Is(execErr, ErrTurnCommitFailed) {
		return false
	}
	if errors.Is(execErr, ErrCallQueuedNotExecuted) || isSessionLockBusyErr(execErr) {
		return false
	}
	var alreadyAttempted AlreadyAttempted
	if errors.As(execErr, &alreadyAttempted) && alreadyAttempted.AlreadyAttempted() {
		return false
	}
	return true
}

// classifyBackgroundOutcome interprets an admitSession release outcome --
// whether obtained by calling executeEntrySync directly, or by waiting on
// another local execution's admissionEntry and reading what IT published --
// into the shapes DrainSessionNow's contract distinguishes. The input is a
// typed admissionOutcome (task #613 of the 2026-08-20 read-only release
// review; previously a bare `error`, which is what let F3 conflate a
// destructive terminal deletion with a harmless early return -- see
// admissionOutcome's own doc), switched on outcome.kind:
//
//   - outcomeNoRowTouched: the admission holder never durably touched any
//     row. Nothing happened, and nothing is known about whether some OTHER
//     row is still pending. Result: (drained=false, rowID="", outcomeErr=nil,
//     stopNow=false) -- fall through and retry admission; the caller that
//     lost this race is not entitled to assume there is nothing left to do
//     just because ONE other holder touched nothing.
//   - outcomeBusy: a genuinely DIFFERENT, live owner holds the session right
//     now (not a same-pump early return). Result: (drained=false, rowID="",
//     outcomeErr=nil, stopNow=true) -- stop immediately; waiting for a
//     stranger's turn is not DrainSessionNow's job.
//   - outcomeTerminalDeleted: task #613/F3's fix. A row was leased and
//     terminal-failed (DELETEd) WITHOUT ever calling executeEntrySync. This
//     is NOT "nothing happened" -- it is a row-scoped, destructive
//     bookkeeping outcome that must reach a waiter as a FAILURE tied to
//     outcome.rowID, exactly like a genuine execution's terminal failure
//     would. Result: (drained=true, rowID=outcome.rowID, outcomeErr=a
//     non-nil error describing the dead-letter (synthesized here if
//     outcome.err was nil, i.e. the terminal DELETE itself succeeded),
//     stopNow=false).
//   - outcomeExecuted: executeEntrySync genuinely ran the row (leased by the
//     admission holder, outcome.rowID always set) and reached SOME
//     resolution -- a clean commit (outcome.err == nil), a terminal
//     AlreadyAttempted failure, ErrTurnCommitFailed, errLeaseLost, or an
//     ordinary retryable failure. Result: (drained=true,
//     rowID=outcome.rowID, outcomeErr=outcome.err, stopNow=false) -- fold
//     the outcome into the caller's row ledger and loop to check for more
//     pending work, exactly as the pre-existing "turn actually ran"
//     handling already did for the executed-here case before this helper
//     existed.
//
// Only outcomeBusy halts DrainSessionNow immediately. outcomeNoRowTouched
// and the two "something happened" kinds all loop -- for entirely different
// reasons (nothing is known yet, vs. something IS known and there may be
// more) -- which is why outcomeNoRowTouched cannot share stopNow=true with
// outcomeBusy despite both being reachable from admission's early-return
// paths.
//
// Centralizing this (task #575 of the 2026-08-19 release-readiness review;
// row identity and the terminal-deletion kind added by task #613 of the
// 2026-08-20 read-only release review) is what makes DrainSessionNow's two
// branches -- "I ran executeEntrySync myself" and "I waited for someone
// else's admission release" -- agree on what a given outcome means. Before
// task #575, only the first branch interpreted the error at all; the second
// treated EVERY admission release as an unconditional success. Before task
// #613, a terminal deletion (outcomeTerminalDeleted) was published through
// the SAME sentinel as outcomeNoRowTouched, which is the F3 defect this
// dedicated kind exists to close.
func classifyBackgroundOutcome(outcome admissionOutcome) (drained bool, rowID string, outcomeErr error, stopNow bool) {
	switch outcome.kind {
	case outcomeNoRowTouched:
		// The admitted caller held the session's admission slot but never
		// durably touched any row -- a busy-backoff/worker-pool-full/
		// raced-lease/no-coordinator/shutdown-nack/ctx-ended-early early
		// return in processEntry, or DrainSessionNow's own "nothing
		// pending" or pre-execution failure paths (see noRowTouched's own
		// doc for the full enumeration).
		//
		// stopNow=false HERE (unlike outcomeBusy below): a session that
		// raced/backed-off/shut-down-nacked away is NOT known to be
		// genuinely, legitimately owned by anyone else -- the row may still
		// be sitting pending for THIS call to pick up itself (e.g. a
		// different pump instance's processEntry lost its own lease
		// attempt to yet a THIRD instance, but the row could equally still
		// be unclaimed, or a completely different entry for the same
		// session could have been enqueued since). A waiting DrainSessionNow
		// call must fall through and retry admission itself -- exactly the
		// behavior promised (and, before task #575's fix, NOT delivered) by
		// admitSession's own release-closure doc and by every early-return
		// call site's comment. Returning immediately here (stopNow=true)
		// would incorrectly treat "nothing happened in THAT attempt" as
		// "nothing left to do", potentially leaving genuinely pending work
		// undrained -- a narrower version of the same class of bug in the
		// opposite direction (a false NEGATIVE instead of a false
		// positive), and not something a caller that merely lost a race
		// should conclude on someone else's behalf.
		return false, "", nil, false

	case outcomeBusy:
		// A genuinely different live owner has this session right now --
		// not this call, not the background tick (see those errors' own
		// docs on executeEntrySync). NOTHING RAN: the call was appended to
		// that owner's mailbox, or the OS session lock was held, and the
		// row was released without an attempt penalty for someone else to
		// pick up later.
		return false, "", nil, true

	case outcomeTerminalDeleted:
		// task #613/F3: a row was leased and terminal-failed (DELETEd)
		// WITHOUT ever calling executeEntrySync -- the attempts-exhausted
		// fast path. This is destructive, row-scoped bookkeeping, not "no
		// row touched": a waiter must learn the row is gone, tied to its
		// own ID, exactly like a genuine execution's terminal failure would
		// be. outcome.err is nil when the terminal DELETE itself succeeded
		// (the row IS a confirmed dead-letter -- nil there is not "nothing
		// happened", it is "the destructive write landed cleanly"), so a
		// non-nil error is synthesized here for the ledger/caller to see;
		// outcome.err is non-nil when the terminal write ITSELF failed,
		// meaning the row's fate is unconfirmed rather than a clean no-op
		// (F3's second bad branch) -- that error is surfaced as-is.
		termErr := outcome.err
		if termErr == nil {
			termErr = fmt.Errorf("run queue entry %q exceeded max attempts and was terminal-failed", outcome.rowID)
		} else {
			termErr = fmt.Errorf("run queue entry %q exceeded max attempts and its terminal-fail write itself failed: %w", outcome.rowID, termErr)
		}
		return true, outcome.rowID, termErr, false

	default: // outcomeExecuted
		// executeEntrySync itself can ALSO return ErrCallQueuedNotExecuted
		// or a *SessionLockBusyError as its own return value: the row was
		// genuinely leased by the admission holder, but Coordinator.Run
		// found the session already owned by a DIFFERENT, live process/turn
		// when it actually tried to run it (see executeEntrySync's own
		// handling of both). That is the SAME "genuinely different live
		// owner" situation outcomeBusy describes at the admission level,
		// just discovered one layer deeper (inside execution instead of
		// before it) -- and it must classify identically (stopNow=true,
		// drained=false), or DrainSessionNow's local-execution loop never
		// stops retrying a session a different owner legitimately holds.
		// This check must run BEFORE the generic "an execution happened"
		// fallthrough below, exactly as it did when this function switched
		// on a bare error via errors.Is instead of outcome.kind.
		if errors.Is(outcome.err, ErrCallQueuedNotExecuted) || isSessionLockBusyErr(outcome.err) {
			return false, "", nil, true
		}

		// Every remaining outcome -- a clean commit (outcome.err == nil), a
		// terminal AlreadyAttempted failure, an ErrTurnCommitFailed (turn
		// ran, Ack did not), errLeaseLost (a new owner now holds the row),
		// or an ordinary retryable failure -- means an execution genuinely
		// happened and reached a resolution in this process, whether or not
		// that resolution was a clean success. The row's own bookkeeping
		// (Ack/Nack/TerminalFail, or nothing at all for errLeaseLost -- see
		// its own doc) was already written by executeEntrySync itself; this
		// call's only remaining job is to make sure its caller learns the
		// truth (tied to outcome.rowID) instead of a fabricated success,
		// and to keep checking for more pending work rather than stopping
		// early.
		return true, outcome.rowID, outcome.err, false
	}
}

// isSessionLockBusyErr reports whether err is (or wraps) a pointer to
// SessionLockBusyError -- the same check executeEntrySync performs inline,
// factored out so DrainSessionNow can use it without duplicating the
// errors.As boilerplate.
func isSessionLockBusyErr(err error) bool {
	var busyErr *SessionLockBusyError
	return errors.As(err, &busyErr)
}
