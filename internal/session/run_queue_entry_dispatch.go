// Entry admission and dispatch: deciding which pending entries this pump
// instance may lease (in-flight guard, busy backoff, bounded worker pool,
// shutdown admission gate), leasing them, and spawning executeEntry.

package session

import (
	"context"
	"fmt"
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
	releaseSession, _, admitted := p.admitSession(entry.SessionID)
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
	go p.executeEntry(p.ctx, leased, releaseSession)
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
func (p *RunQueuePump) executeEntry(ctx context.Context, leased *RunQueueEntry, releaseSession func(outcome admissionOutcome)) {
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
	defer func() {
		if r := recover(); r != nil {
			releaseSession(executedOutcome(leased.ID, fmt.Errorf("run_queue_pump: executeEntrySync panicked: %v", r)))
			panic(r) // preserve normal panic propagation/crash behavior
		}
		releaseSession(executedOutcome(leased.ID, execErr))
	}()

	// executeEntrySync's return value already carries everything any
	// interested party could need to know about this row's outcome — see
	// its own doc. This detached background path has nobody of its own to
	// report to (every outcome that CAN be written for the row already was,
	// inside executeEntrySync), but a concurrent DrainSessionNow for the
	// SAME session may be parked on this admission's done channel waiting
	// for exactly this value, so it is always published via releaseSession,
	// never silently discarded.
	execErr = p.executeEntrySync(ctx, leased)
}
