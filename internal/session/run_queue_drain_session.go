// Synchronous drain: DrainSessionNow lets a short-lived process finish
// pending durable entries for a session in-process instead of leaving them
// for some future background tick, plus its lock-busy error helper.

package session

import (
	"context"
	"fmt"
	"log/slog"
)

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

	// sawOtherAdmission (task #624, F-5 of the 2026-08-20 ninth review) is
	// set the first time this call loses the admission race and WAITS on
	// another holder's admissionEntry for this session. It distinguishes
	// the two shapes an outstanding-entries finding can arrive in at the
	// bottom of the loop when nothing has executed yet:
	//
	//   - This call OBSERVED a live, same-pump admission for the session
	//     (sawOtherAdmission == true): any outstanding leased row is
	//     plausibly the one that admission's holder is working through
	//     right now -- reporting it as this call's failure is the exact
	//     false DrainNoWork-to-DrainFailed conversion the anyExecuted gate
	//     was added to prevent (pinned by
	//     TestProcessEntry_RacedLeaseNil_DoesNotFalselyDrainAWaiter).
	//   - This call never saw ANY admission for the session
	//     (sawOtherAdmission == false): nothing in this pump instance is
	//     known to be working on the session, so a row sitting in 'leased'
	//     (or 'pending') belongs to an owner this call has no visibility
	//     into at all -- a foreign process, or an owner that died (e.g. a
	//     prior DrainSessionNow whose executeEntrySync panicked and never
	//     Nack'd) leaving an orphaned lease behind. Pre-#624, the
	//     anyExecuted gate alone made that row INVISIBLE: the
	//     outstanding-entries query was skipped entirely, and this call
	//     reported (DrainNoWork, nil) over a durable row whose fate it
	//     never even looked at.
	var sawOtherAdmission bool
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
			sawOtherAdmission = true

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

			// Task #616/P2-1 (2026-08-20 read-only release review): the three
			// releases below -- workerWg.Done, the execSem slot, and the
			// session admission -- used to run only AFTER executeEntrySync
			// returned normally, unlike executeEntry (run_queue_entry_dispatch.go),
			// whose equivalent background path wraps the same three releases
			// in a defer specifically so a panic inside executeEntrySync still
			// unwinds them. A panic here currently crashes the whole process
			// (Go's default handler), so today there is no OBSERVABLE gap --
			// but if recovery is ever added at any outer boundary (a test
			// harness, a future top-level recover in RunNonInteractive, etc.),
			// an unrecovered panic on this synchronous path would otherwise
			// leak workerWg's count (hanging a future Stop() forever), leave
			// execSem permanently short one slot, and strand any concurrent
			// DrainSessionNow waiting on this session's admissionEntry.done
			// forever, since nothing would ever close it or record an
			// outcome. Scoped in a defer, exactly mirroring executeEntry's own
			// panic-recovery shape: publish executedOutcome(leased.ID, ...) so
			// a waiter learns this row's fate instead of hanging, then
			// re-panic to preserve normal crash/propagation behavior -- this
			// function's job is only to make the release order panic-safe,
			// not to swallow the panic.
			var execErr error
			func() {
				defer p.workerWg.Done()
				defer func() { <-p.execSem }()
				defer func() {
					if r := recover(); r != nil {
						releaseSession(executedOutcome(leased.ID, fmt.Errorf("run_queue_pump: executeEntrySync panicked: %v", r)))
						panic(r) // preserve normal panic propagation/crash behavior
					}
					releaseSession(executedOutcome(leased.ID, execErr))
				}()
				execErr = p.executeEntrySync(ctx, leased)
			}()

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
		// `rush run` had already exited 0 over durable, unconfirmed work.
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
		// Gated on THREE conditions, all required, mirroring
		// verdictOnCtxDone's own anyExecuted gate a few dozen lines above
		// (that function's doc explains why the analogous mistake there --
		// recording unconditionally -- turned a genuine DrainNoWork into a
		// false DrainFailed; the same shape of mistake is possible here):
		//
		//   - ledger.anyExecuted || !sawOtherAdmission (task #624/F-5 relaxed
		//     this from a bare anyExecuted): when NOTHING has executed in
		//     this call, the query still runs UNLESS this call observed a
		//     same-pump admission for the session (see sawOtherAdmission's
		//     own doc). The bare anyExecuted gate -- added with task #610 to
		//     keep a losing admission race's correct DrainNoWork intact --
		//     also hid a row this call has NO live local explanation for at
		//     all: a leased row orphaned by a dead owner (the reviewer's
		//     executed probe: a row left leased after a panic never enters
		//     the drain's field of view, independent of what the caller maps
		//     the result to). That cold-call case is now visible -- the
		//     query runs, and an outstanding row is reported through
		//     DrainFailed exactly as task #610 already reports it for a call
		//     that DID execute -- while the raced-admission case keeps its
		//     deliberate DrainNoWork: this call waited on the admission
		//     holder, so an outstanding row is not silently orphaned from
		//     its point of view. That distinction is pinned on BOTH sides:
		//     TestProcessEntry_RacedLeaseNil_DoesNotFalselyDrainAWaiter
		//     (observed admission => still DrainNoWork) and this file's own
		//     #624/F-5 regression test (no admission ever observed =>
		//     DrainFailed), so neither half can silently regress alone.
		//
		//     The cross-process LIVE-owner case is REACHABLE and
		//     deliberately converts too (task #624 follow-up review):
		//     LeaseRunQueueEntry is a pure DB write (leased_by = ?, see
		//     sql/run_queue.sql) that never consults the OS session lock --
		//     that lock is acquired one layer up, inside Coordinator.Run
		//     (agent_run.go), and task #622 exists precisely because the
		//     lock is not consulted everywhere it should be. A different
		//     process's pump (the DB is shared per workspace) or a second
		//     pump instance in this same process (p.inFlight is per-pump)
		//     can therefore hold a live lease on a row of this session
		//     with no admission this call could ever observe, and this
		//     call reports that row as DrainFailed/
		//     ErrOutstandingRunQueueEntry. That is not a new decision:
		//     task #610's own fix already reports exactly that shape (a
		//     live foreign owner, unexpired lease) as DrainFailed whenever
		//     anyExecuted is true -- see
		//     TestDrainSessionNow_ForeignLeasedRow_NeverReportsComplete,
		//     whose foreign lease carries a 5-minute TTL. #624/F-5 only
		//     extends the same reporting to the cold arm, because the
		//     row's fate is equally unknown to this call whether or not
		//     this call also executed some OTHER row first. Narrowing this
		//     arm to "provably unowned rows only" was considered and ruled
		//     out: the only query available
		//     (HasOutstandingRunQueueEntriesForSession) deliberately does
		//     not look at lease liveness -- an earlier round decided an
		//     expired-but-not-swept lease still counts as outstanding --
		//     so such narrowing would require reintroducing exactly the
		//     liveness judgment that decision rejected.
		//   - !ledger.hasFailures(): if the ledger already has a surviving
		//     failure recorded, verdict(false) below is already going to
		//     return DrainFailed regardless of what this check finds
		//     (rowLedger.verdict checks len(failed) > 0 before it ever looks
		//     at contended), so querying here would only ever add a
		//     REDUNDANT synthetic entry that could SHADOW an
		//     already-diagnosed, row-specific cause via mostRecentFailure's
		//     priority rule -- e.g. row A's specific
		//     ErrTurnCommitFailed/errLeaseLost/AlreadyAttempted failure,
		//     still sitting in the ledger under A's own ID precisely so a
		//     caller (and this package's own tests) can see it, getting
		//     replaced in the reported error by a generic "still
		//     outstanding" message that names no specific row and carries
		//     no specific cause. hasFailures() is the same len(failed) > 0
		//     test verdict() itself performs, kept in sync with it
		//     deliberately (see hasFailures' own doc) rather than re-derived
		//     here.
		if (ledger.anyExecuted || !sawOtherAdmission) && !ledger.hasFailures() {
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
