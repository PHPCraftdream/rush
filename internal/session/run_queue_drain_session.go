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
// Returns drained=true if at least one entry was observed to execute for
// this session during the call — either leased and run by this call
// directly, or (see below) by this pump's own background tick racing ahead
// of it. drained=false, err=nil means nothing was pending; callers must
// NOT treat that as having recovered anything (a plain user-initiated
// cancel/timeout with no durable continuation looks identical to "nothing
// to drain" and must be left as the caller's original outcome).
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

		leased, leaseErr := p.cfg.Sessions.LeaseRunQueueEntry(ctx, sessionID, p.cfg.PumpInstanceID, p.leaseTTL())
		if leaseErr != nil {
			return drained, leaseErr
		}

		if leased != nil {
			drained = true

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
				continue
			}

			p.inFlightMu.Lock()
			p.inFlight[sessionID] = struct{}{}
			p.inFlightMu.Unlock()

			select {
			case p.execSem <- struct{}{}:
			case <-ctx.Done():
				// Release the lease we just took rather than leaving it
				// leased with nobody executing it. A later tick (this
				// process or another) recovers it via the ordinary
				// lease-expiry path regardless, but releasing promptly
				// avoids waiting out a full TTL for no reason.
				p.inFlightMu.Lock()
				delete(p.inFlight, sessionID)
				p.inFlightMu.Unlock()
				nackCtx, nackCancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
				if nackErr := p.cfg.Sessions.NackRunQueueEntryNoAttemptPenalty(nackCtx, leased.ID, p.cfg.PumpInstanceID, "run_queue_pump: DrainSessionNow's ctx ended before an execution slot was available"); nackErr != nil {
					slog.Error("run_queue_pump: DrainSessionNow release-on-ctx-done nack failed", "id", leased.ID, "session_id", sessionID, "err", nackErr, "instance_id", p.cfg.PumpInstanceID)
				}
				nackCancel()
				return drained, ctx.Err()
			}

			execErr := p.executeEntrySync(ctx, leased)

			<-p.execSem
			p.inFlightMu.Lock()
			delete(p.inFlight, sessionID)
			p.inFlightMu.Unlock()

			if errors.Is(execErr, ErrCallQueuedNotExecuted) || isSessionLockBusyErr(execErr) {
				// A genuinely different live owner has this session right
				// now — not this call, not the background tick (see those
				// errors' own docs on executeEntrySync). Waiting for a
				// stranger's turn to finish is not this call's job: stop
				// and let the caller's original outcome stand for
				// whatever wasn't drained.
				return drained, nil
			}
			err = execErr
			continue // more may be pending (e.g. a second stacked interrupt) — loop and check again
		}

		// Nothing pending for THIS call to lease. Someone else in this
		// pump instance — the background tick, racing ahead of us — might
		// already be executing this session's entry; if so, wait for it
		// to finish and re-check, since its outcome matters exactly as
		// much as this call's own would (see the race note in the doc
		// comment above).
		p.inFlightMu.Lock()
		_, busy := p.inFlight[sessionID]
		p.inFlightMu.Unlock()
		if !busy {
			return drained, err
		}
		drained = true
		select {
		case <-ctx.Done():
			return drained, ctx.Err()
		case <-time.After(p.drainSessionPollInterval()):
		}
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
