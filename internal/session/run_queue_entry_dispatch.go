// Entry admission and dispatch: deciding which pending entries this pump
// instance may lease (in-flight guard, busy backoff, bounded worker pool,
// shutdown admission gate), leasing them, and spawning executeEntry.

package session

import (
	"context"
	"log/slog"
	"time"
)

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
func (p *RunQueuePump) processEntry(ctx context.Context, entry *RunQueueEntry) {
	// Refuse to lease ANY entry for a session this pump instance already
	// has an executeEntry goroutine running for — see the inFlight field's
	// doc for why (self-inflicted lease-expiry race, and same-tick
	// same-session double dispatch). Left pending; a later tick retries
	// once that goroutine finishes and releases the session.
	p.inFlightMu.Lock()
	_, busy := p.inFlight[entry.SessionID]
	p.inFlightMu.Unlock()
	if busy {
		slog.Debug("run_queue_pump: session already has an execution in flight from this pump, deferring", "id", entry.ID, "session_id", entry.SessionID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	// Refuse to lease a pending entry for a session this pump instance
	// backed off from after an ErrCallQueuedNotExecuted outcome — see the
	// busyBackoffUntil field's doc. Without this, the entry (immediately
	// visible again as 'pending', by design — no attempt penalty) would be
	// re-leased and re-dispatched on the very next tick, appending another
	// duplicate call to the same busy owner's mailbox.
	p.busyBackoffMu.Lock()
	until, backingOff := p.busyBackoffUntil[entry.SessionID]
	if backingOff && !time.Now().Before(until) {
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
		if err := p.cfg.Sessions.TerminalFailRunQueueEntry(ctx, leased.ID, p.cfg.PumpInstanceID); err != nil {
			slog.Error("run_queue_pump: terminal fail failed", "id", leased.ID, "session_id", leased.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		}
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

	// Mark this session in flight BEFORE dispatching, under the same lock
	// processEntry's own busy-check above uses — closes the check-then-act
	// window between that check and this dispatch (both run synchronously
	// within tick()'s single-threaded per-entry loop, so there is no
	// concurrent processEntry call to race against, but the mark must still
	// land before executeEntry's goroutine can possibly finish and unmark).
	p.inFlightMu.Lock()
	p.inFlight[leased.SessionID] = struct{}{}
	p.inFlightMu.Unlock()

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
		// inFlight was marked above (before this gate); it is only ever
		// cleared by executeEntry's defer, which will now never run for
		// this entry, so clear it here or the session would appear
		// permanently busy to this pump instance.
		p.inFlightMu.Lock()
		delete(p.inFlight, leased.SessionID)
		p.inFlightMu.Unlock()
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

	// Execute the call (detached, not blocking this pump tick)
	go p.executeEntry(ctx, leased)
}

// executeEntry runs a leased entry and handles success/failure. Called
// detached (go executeEntry(...)) by the background tick's processEntry,
// which has already reserved this call's execSem slot and workerWg
// registration and marked the session inFlight before dispatching — this
// releases all three via defer regardless of outcome, matching the
// admission it assumes was already granted.
func (p *RunQueuePump) executeEntry(ctx context.Context, leased *RunQueueEntry) {
	defer p.workerWg.Done()

	// Release the semaphore slot when execution completes (P1-4).
	// This guarantees the slot is returned even on panic or any error path.
	defer func() { <-p.execSem }()

	defer func() {
		p.inFlightMu.Lock()
		delete(p.inFlight, leased.SessionID)
		p.inFlightMu.Unlock()
	}()

	// The returned error is deliberately discarded here: the background
	// tick's own outcome handling (Ack/Nack/TerminalFail) already happened
	// inside executeEntrySync, and nothing on this detached path needs to
	// inspect the result further. DrainSessionNow (task #421/P0-1) is the
	// caller that DOES need it, to decide whether to keep draining or
	// surface a failure — see there.
	_ = p.executeEntrySync(ctx, leased)
}
