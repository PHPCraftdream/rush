package session

// Classifying what a single execution attempt's error means for the drain:
// whether it is an ordinary retryable outcome worth re-checking, how a
// background admission outcome maps onto the ledger, and the
// session-lock-busy predicate. Split out of run_queue_drain_session.go when
// the 1000-line file limit landed.

import (
	"errors"
	"fmt"
)

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
