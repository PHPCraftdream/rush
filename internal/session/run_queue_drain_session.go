// Synchronous drain: DrainSessionNow lets a short-lived process finish
// pending durable entries for a session in-process instead of leaving them
// for some future background tick, plus its lock-busy error helper.

package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// DrainSessionNow synchronously executes every currently-pending run-queue
// entry for sessionID, blocking the caller until the session's durable
// queue is empty and no execution of it is in flight in this pump instance
// — or until ctx is done.
//
// It exists so a short-lived process (crush run) can finish a durable
// continuation in the SAME process instead of leaving it for some future
// invocation's background tick to eventually pick up (task #421/P0-1): a
// cross-process interrupt landing on a busy session cancels the in-flight
// generation and durably enqueues its replacement (handleInterruptTick,
// mailbox.go's FromDurableQueue guard), deliberately WITHOUT a live
// mb.replacement handoff — the durable row is the only remaining owner.
// Without something calling this, that row sits pending until the
// background pump's next tick (RunQueuePumpInterval, 3s in production)
// happens to fire before the process exits — a race the process routinely
// loses, since RunNonInteractive's own completion path runs in
// milliseconds after the cancellation.
//
// Returns drained=true only if a continuation actually EXECUTED in this
// process during the call — leased and run to completion by this call, or
// (see below) run by this pump's own background tick racing ahead of it.
// Leasing a row is not executing it, and this distinction is the whole of
// P0-1 in the 2026-08-18 release-readiness review: drained used to be set
// the moment LeaseRunQueueEntry returned a row, before any execution was
// attempted. When the execution then turned out to be impossible because a
// genuinely different live owner held the session (ErrCallQueuedNotExecuted
// or *SessionLockBusyError), the row was correctly released without an
// attempt penalty — and the function still answered (true, nil).
// RunNonInteractive reads that pair as "the continuation ran here" and
// converts the operator's cancelled run into exit code 0 with a success
// envelope, while the work is still queued and will run later, in another
// process, after the user was told it had finished.
//
// drained=false, err=nil means nothing ran and nothing failed; callers must
// NOT treat that as having recovered anything (a plain user-initiated
// cancel/timeout with no durable continuation looks identical to "nothing
// to drain", and both must leave the caller's original outcome standing).
//
// Race against the background tick: LeaseRunQueueEntry is atomic at the DB
// level, so two callers racing for the same row can never both execute it
// — but if the background tick wins the race, THIS call's own lease
// attempt simply finds nothing pending, even though the row is genuinely
// being executed right now by a goroutine this call didn't start. Silently
// returning "nothing to drain" in that case would reproduce the exact bug
// this function exists to close, just via a race instead of a certainty.
// The fix: check p.inFlight for this session before concluding there is
// nothing left to wait for. If busy, that busy state was set either by
// this call's own leasing branch below (mirroring processEntry) or by the
// background tick's processEntry/executeEntry — either way, poll (bounded,
// drainSessionPollInterval) until it clears, then loop back and check
// again for anything newly pending, rather than trying to coordinate a
// clean handoff between the two paths.
//
// Deliberately does NOT replicate processEntry's RunQueueMaxAttempts
// pre-check, busyBackoffUntil dedup, or admitMu/stopping shutdown gate —
// those exist for the long-running, many-tick background scenario. A
// synchronous drain bounded by the caller's own ctx (crush run's --timeout)
// does not need them: a genuinely stuck or poison entry hits ctx's
// deadline (or, for attempts, the loop below still honors
// RunQueueMaxAttempts directly so a truly poison entry terminal-fails
// instead of being retried forever inside one call) and this call returns
// with that error rather than looping unboundedly.
func (p *RunQueuePump) DrainSessionNow(ctx context.Context, sessionID string) (drained bool, err error) {
	for {
		if ctx.Err() != nil {
			return drained, ctx.Err()
		}

		// Reserve the session through the SAME atomic gate the background
		// tick uses, and do it BEFORE leasing (P1-1 of the 2026-08-18
		// release-readiness review). This call used to lease first and then
		// assign the shared marker unconditionally, which let a drain start
		// row B for a session a background worker was already executing row
		// A for — two executions, one boolean, and whichever finished first
		// cleared it. See admitSession.
		releaseSession, admitted := p.admitSession(sessionID)
		if !admitted {
			// Something in THIS pump instance is already executing for this
			// session — the background tick, having won the lease race. Wait
			// for it and re-check, because its outcome matters exactly as
			// much as this call's own would: its messages reach the caller
			// through the same subscription.
			//
			// This is the wait that used to sit at the bottom of the loop,
			// reached only after a lease attempt came back empty. Hoisting it
			// above the lease is what removes the window the old ordering
			// left open.
			//
			// The one thing this branch cannot see is that other execution's
			// OUTCOME: if its executeEntrySync returns
			// ErrCallQueuedNotExecuted, nothing actually ran and this still
			// reports drained=true. That residual is much narrower than the
			// P0-1 case (it needs the background tick to win the lease race
			// AND then find the session externally owned), and closing it
			// needs an outcome handoff between the two paths, not a flag.
			drained = true
			select {
			case <-ctx.Done():
				return drained, ctx.Err()
			case <-time.After(p.drainSessionPollInterval()):
			}
			continue
		}

		leased, leaseErr := p.cfg.Sessions.LeaseRunQueueEntry(ctx, sessionID, p.cfg.PumpInstanceID, p.leaseTTL())
		if leaseErr != nil {
			releaseSession()
			return drained, leaseErr
		}

		if leased != nil {
			// NOTE: drained is deliberately NOT set here. Holding a lease is
			// not the same as having executed anything — see the P0-1 note in
			// this function's doc. It is set below, once executeEntrySync has
			// actually run a turn.

			// Mirrors processEntry's own attempts-exhausted check (see
			// there) — this call bypassed that check by leasing directly
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
				err = fmt.Errorf("run queue entry %q exceeded max attempts", leased.ID)
				releaseSession()
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
				releaseSession()
				nackCtx, nackCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
				if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(nackCtx, leased.ID, p.cfg.PumpInstanceID, "run_queue_pump: DrainSessionNow's ctx ended before an execution slot was available"); nackErr != nil {
					slog.Error("run_queue_pump: DrainSessionNow release-on-ctx-done nack failed", "id", leased.ID, "session_id", sessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
				}
				nackCancel()
				return drained, ctx.Err()
			}

			// Register with workerWg for the execution, so Stop() waits for
			// this drain exactly as it waits for a background worker.
			//
			// Without it (P0-2 of the 2026-08-18 release-readiness review)
			// Stop() could see an idle pump while this call was mid-turn,
			// and App.Shutdown would then close the database underneath a
			// live execution. The Add happens under admitMu, matching the
			// ordering processEntry relies on: Stop sets `stopping` under
			// that same mutex before calling Wait, so an Add can never race
			// a Wait that has already begun.
			//
			// If a stop is already underway the lease is released instead of
			// executed — starting a turn that Stop is known not to wait for
			// is how the shutdown hole reopens.
			p.admitMu.Lock()
			stopping := p.stopping
			if !stopping {
				p.workerWg.Add(1)
			}
			p.admitMu.Unlock()

			if stopping {
				releaseSession()
				<-p.execSem
				nackCtx, nackCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
				if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(nackCtx, leased.ID, p.cfg.PumpInstanceID, "run_queue_pump: DrainSessionNow declined to start because the pump is stopping"); nackErr != nil {
					slog.Error("run_queue_pump: DrainSessionNow release-on-stopping nack failed", "id", leased.ID, "session_id", sessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
				}
				nackCancel()
				return drained, ErrCallQueuedNotExecuted
			}

			execErr := p.executeEntrySync(ctx, leased)
			p.workerWg.Done()

			<-p.execSem
			releaseSession()

			if errors.Is(execErr, ErrCallQueuedNotExecuted) || isSessionLockBusyErr(execErr) {
				// A genuinely different live owner has this session right
				// now — not this call, not the background tick (see those
				// errors' own docs on executeEntrySync). NOTHING RAN here:
				// the call was appended to that owner's mailbox, or the OS
				// session lock was held, and the row was released without an
				// attempt penalty for someone else to pick up later.
				//
				// So drained stays as it was — false, unless an EARLIER
				// iteration of this loop genuinely executed something. That
				// is the P0-1 fix: this branch used to return (true, nil)
				// because drained had already been set by the mere act of
				// leasing, and the caller turned a cancelled run into a
				// success for work that had not run and would not run here.
				//
				// Waiting for a stranger's turn to finish is not this call's
				// job either: stop, and let the caller's original outcome
				// stand for whatever wasn't drained.
				return drained, nil
			}

			// The turn actually ran. Either it committed (execErr == nil) or
			// it failed in a way that already wrote its own outcome for the
			// row; both are executions that happened in this process, and
			// both produced messages the caller has already seen. err
			// carries the distinction upward.
			drained = true
			err = execErr
			continue // more may be pending (e.g. a second stacked interrupt) — loop and check again
		}

		// Nothing pending, and nothing else can be executing for this
		// session either — this call currently holds its only admission.
		//
		// The "is someone else busy with it?" check used to live HERE,
		// reached only after a lease attempt came back empty, which is
		// exactly what made the old ordering racy: by then this call had
		// already leased and marked. It now runs before leasing, at the top
		// of the loop.
		releaseSession()
		return drained, err
	}
}

// isSessionLockBusyErr reports whether err is (or wraps) a
// *SessionLockBusyError — the same check executeEntrySync performs inline,
// factored out so DrainSessionNow can use it without duplicating the
// errors.As boilerplate.
func isSessionLockBusyErr(err error) bool {
	var busyErr *SessionLockBusyError
	return errors.As(err, &busyErr)
}
