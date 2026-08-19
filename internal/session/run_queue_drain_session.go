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
// Race against the background tick: LeaseRunQueueEntry is atomic at the DB
// level, so two callers racing for the same row can never both execute it
// -- but if the background tick wins the race, THIS call's own lease
// attempt simply finds nothing pending, even though the row is genuinely
// being executed right now by a goroutine this call didn't start. Silently
// returning "nothing to drain" in that case would reproduce the exact bug
// this function exists to close, just via a race instead of a certainty.
// The fix: check admission for this session before concluding there is
// nothing left to wait for. If busy, wait for that specific execution's
// admissionEntry.done (bounded by ctx), then process its published outcome
// through classifyBackgroundOutcome -- the same code path this function's
// own leasing branch uses for an execution it ran itself.
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
			result, verdictErr := ledger.verdict(false)
			if result == DrainNoWork {
				// Preserve the historical "report ctx.Err() directly when
				// nothing has happened yet" behavior rather than a bare
				// DrainNoWork/nil -- a caller cancelling THIS call's own ctx
				// before anything ran still needs to see that as an error,
				// not silence.
				return DrainNoWork, ctx.Err()
			}
			return result, verdictErr
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
				result, verdictErr := ledger.verdict(false)
				if result == DrainNoWork {
					return DrainNoWork, ctx.Err()
				}
				return result, verdictErr
			case <-otherEntry.done:
			}

			// otherEntry.err is now safe to read: done's close happens-after
			// the write (see admissionEntry's own doc). Classify it through
			// the exact same helper the executed-here branch below uses, and
			// apply it with the exact same loop-vs-return shape: only the
			// "busy, nothing ran" outcome returns immediately (see
			// classifyBackgroundOutcome's own doc for why every other
			// outcome instead loops back to check for more pending work).
			outcomeDrained, outcomeErr, stopNow := classifyBackgroundOutcome(otherEntry.err)
			if outcomeDrained {
				// This outcome came from admissionEntry.done, which only
				// closes AFTER the other execution's own Ack/Nack/
				// TerminalFail write has already landed (see
				// admissionEntry's doc) -- it is a fully resolved, known
				// outcome. It is NOT, however, a row this call leased
				// itself, so there is no ID here this call could use to
				// later supersede it via its OWN retry -- record it as
				// unattributed rather than either fabricating an ID or
				// (the pre-#592 bug) letting it share the single
				// session-wide 'err' slot a later, unrelated row could
				// silently overwrite.
				if outcomeErr != nil {
					ledger.recordUnattributed(outcomeErr)
				} else {
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
			// never reached, so nothing executed. Publish
			// errNoExecutionAttempted, not leaseErr itself: leaseErr is
			// non-nil, but classifyBackgroundOutcome does not treat "err is
			// non-nil" as the signal for "nothing happened" -- only
			// ErrCallQueuedNotExecuted/SessionLockBusyError and
			// errNoExecutionAttempted map to stopNow/no-drain. Publishing
			// leaseErr directly would fall through to
			// classifyBackgroundOutcome's default branch and tell a waiting
			// DrainSessionNow that an execution genuinely happened here,
			// which is false -- this call's own return value (leaseErr,
			// non-nil) is what correctly reports the failure to THIS call's
			// own caller; the outcome published to a DIFFERENT, waiting
			// caller must instead say "nothing ran, retry admission".
			releaseSession(errNoExecutionAttempted)
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
				// This row was terminal-failed by a DIRECT write here, not
				// by executeEntrySync -- mirrors processEntry's own
				// attempts-exhausted branch, which never calls
				// executeEntrySync either and releases with
				// errNoExecutionAttempted for the same reason (see that
				// sentinel's doc). A waiter must not be told an execution
				// happened: this call's OWN ledger already carries the
				// max-attempts failure for its own caller; what a
				// DIFFERENT, waiting caller needs is "nothing ran, retry
				// admission and look for other pending work".
				releaseSession(errNoExecutionAttempted)
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
				// waiter must be told errNoExecutionAttempted, not ctx.Err()
				// itself: like the leaseErr case above, a non-nil error here
				// is not the signal classifyBackgroundOutcome looks for.
				releaseSession(errNoExecutionAttempted)
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
				releaseSession(ErrCallQueuedNotExecuted)
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
			releaseSession(execErr)

			// Classify through the same helper the wait branch above uses.
			// Only the "busy, nothing ran" outcome returns immediately --
			// every other outcome (success, terminal failure, failed Ack,
			// lost lease, ordinary retryable failure) means an execution
			// genuinely happened, so the ledger is updated and the loop
			// continues to check for more pending work (e.g. a second
			// stacked interrupt), exactly as the pre-existing "turn actually
			// ran" branch below already did before this outcome could also
			// arrive via a wait.
			outcomeDrained, outcomeErr, stopNow := classifyBackgroundOutcome(execErr)
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
		// errNoExecutionAttempted, not nil: nil is executeEntrySync's own
		// return value for a clean commit, and a nil outcome published here
		// would be indistinguishable from one to a second, concurrent
		// DrainSessionNow call that lost the admission race against THIS
		// call and is waiting on it -- telling that waiter a continuation
		// completed when this call in fact found nothing pending at all
		// (the coordinator's concrete false-success scenario for task #575:
		// a second process leases the row first, this call's own leased ==
		// nil branch below is what runs, and the OLD nil here is exactly
		// what a waiting drain would misread as success).
		releaseSession(errNoExecutionAttempted)

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
		//   - still present and 'leased' by a DIFFERENT leasedBy: someone
		//     else has it right now and its outcome is genuinely UNKNOWN to
		//     this call -- reported through errLeaseLost's own "nothing
		//     further can be learned from this attempt" meaning.
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
// into the three shapes DrainSessionNow's contract distinguishes. The input
// is NOT always an executeEntrySync return value: it may also be
// errNoExecutionAttempted, published by an admission holder that never
// called executeEntrySync at all (see that sentinel's own doc) -- the three
// input categories below are mutually exclusive and checked in this order:
//
//   - errNoExecutionAttempted (own branch, checked first): the admission
//     holder never called executeEntrySync. Nothing happened, and nothing
//     is known about whether the row is still pending. Result:
//     (drained=false, outcomeErr=nil, stopNow=false) -- fall through and
//     retry admission; the caller that lost this race is not entitled to
//     assume there is nothing left to do just because ONE other holder did
//     nothing.
//   - ErrCallQueuedNotExecuted / SessionLockBusyError: a genuinely
//     DIFFERENT, live owner holds the session right now (not a same-pump
//     early return). Result: (drained=false, outcomeErr=nil, stopNow=true)
//     -- stop immediately; waiting for a stranger's turn is not
//     DrainSessionNow's job.
//   - everything else (a clean commit via nil, a terminal AlreadyAttempted
//     failure, ErrTurnCommitFailed, errLeaseLost, or an ordinary retryable
//     failure): executeEntrySync genuinely ran and reached SOME resolution.
//     Result: (drained=true, outcomeErr=execErr, stopNow=false) -- fold the
//     outcome into the caller's row ledger and loop to check for more
//     pending work, exactly as the pre-existing "turn actually ran"
//     handling already did for the executed-here case before this helper
//     existed.
//
// Only the middle category (busy) halts DrainSessionNow immediately.
// errNoExecutionAttempted and "an execution happened" both loop -- for
// entirely different reasons (nothing is known yet, vs. something IS known
// and there may be more) -- which is why they cannot share stopNow=true
// despite both being reachable from admission's early-return paths.
//
// Centralizing this (task #575 of the 2026-08-19 release-readiness review,
// both the initial pass and the coordinator's follow-up review that found
// admission's early-return paths were publishing a bare nil instead of a
// dedicated sentinel) is what makes DrainSessionNow's two branches -- "I ran
// executeEntrySync myself" and "I waited for someone else's admission
// release" -- agree on what a given outcome means. Before this helper
// existed, only the first branch interpreted the error at all; the second
// treated EVERY admission release (including terminal failure, a failed
// Ack, a lost lease, AND a bare nil from an early return that never
// executed anything) as an unconditional success, because it never looked
// at the outcome in the first place -- see DrainSessionNow's own doc for the
// enumerated false successes this closes.
func classifyBackgroundOutcome(execErr error) (drained bool, outcomeErr error, stopNow bool) {
	if errors.Is(execErr, errNoExecutionAttempted) {
		// The admitted caller held the session's admission slot but never
		// called executeEntrySync at all -- a busy-backoff/worker-pool-full/
		// raced-lease/attempts-exhausted/shutdown-nack/ctx-ended-early early
		// return in processEntry, or DrainSessionNow's own "nothing
		// pending" or pre-execution failure paths (see
		// errNoExecutionAttempted's own doc for the full enumeration). This
		// is deliberately its own branch, checked BEFORE the "did an
		// execution happen" fallthrough below: nil and
		// errNoExecutionAttempted must never be conflated, because nil is
		// ALSO executeEntrySync's own return value for a clean commit, and
		// treating a bare nil as "nothing happened" would require every
		// call site to remember never to pass literal nil for "no
		// execution" -- exactly the mistake task #575's coordinator review
		// caught.
		//
		// stopNow=false HERE (unlike the busy case immediately below,
		// which is stopNow=true): a session that raced/backed-off/
		// shut-down-nacked away is NOT known to be genuinely,
		// legitimately owned by anyone else -- the row may still be
		// sitting pending for THIS call to pick up itself (e.g. a
		// different pump instance's processEntry lost its own lease
		// attempt to yet a THIRD instance, but the row could equally
		// still be unclaimed, or a completely different entry for the
		// same session could have been enqueued since). A waiting
		// DrainSessionNow call must fall through and retry admission
		// itself -- exactly the behavior promised (and, before this fix,
		// NOT delivered) by admitSession's own release-closure doc and by
		// every early-return call site's comment. Returning immediately
		// here (stopNow=true) would incorrectly treat "nothing happened
		// in THAT attempt" as "nothing left to do", potentially leaving
		// genuinely pending work undrained -- a narrower version of the
		// same class of bug in the opposite direction (a false NEGATIVE
		// instead of a false positive), and not something a caller that
		// merely lost a race should conclude on someone else's behalf.
		return false, nil, false
	}

	if errors.Is(execErr, ErrCallQueuedNotExecuted) || isSessionLockBusyErr(execErr) {
		// A genuinely different live owner has this session right now --
		// not this call, not the background tick (see those errors' own
		// docs on executeEntrySync). NOTHING RAN: the call was appended to
		// that owner's mailbox, or the OS session lock was held, and the
		// row was released without an attempt penalty for someone else to
		// pick up later.
		return false, nil, true
	}

	// Every remaining outcome -- a clean commit (execErr == nil), a terminal
	// AlreadyAttempted failure, an ErrTurnCommitFailed (turn ran, Ack did
	// not), errLeaseLost (a new owner now holds the row), or an ordinary
	// retryable failure -- means an execution genuinely happened and reached
	// a resolution in this process, whether or not that resolution was a
	// clean success. The row's own bookkeeping (Ack/Nack/TerminalFail, or
	// nothing at all for errLeaseLost -- see its own doc) was already
	// written by executeEntrySync itself; this call's only remaining job is
	// to make sure its caller learns the truth instead of a fabricated
	// success, and to keep checking for more pending work rather than
	// stopping early.
	return true, execErr, false
}

// isSessionLockBusyErr reports whether err is (or wraps) a pointer to
// SessionLockBusyError -- the same check executeEntrySync performs inline,
// factored out so DrainSessionNow can use it without duplicating the
// errors.As boilerplate.
func isSessionLockBusyErr(err error) bool {
	var busyErr *SessionLockBusyError
	return errors.As(err, &busyErr)
}
