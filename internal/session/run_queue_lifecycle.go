// Pump lifecycle: Start/Stop (idempotent start, graceful shutdown via the
// admission gate and a unified 5s grace window) plus the run/tick loop
// that drives periodic scans of the durable run queue.

package session

import (
	"context"
	"log/slog"
	"time"
)

// Start begins the pump goroutine. Safe to call multiple times (idempotent).
func (p *RunQueuePump) Start() {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if p.started {
		return
	}
	p.started = true

	p.wg.Add(1)
	go p.run()
	slog.Info("run_queue_pump: started", "instance_id", p.cfg.PumpInstanceID)
}

// Stop gracefully shuts down the pump goroutine.
// Waits for all in-flight executeEntry workers to finish with a 5-second grace period.
// Returns true if shutdown was forced (workers still running after grace period),
// false if all workers finished gracefully.
// Pattern: matches internal/agent/agent.go CancelAll's 5-second grace + stillBusy return.
func (p *RunQueuePump) Stop() bool {
	p.startMu.Lock()
	defer p.startMu.Unlock()
	if !p.started {
		return false
	}

	// Step 0: flip the admission gate BEFORE cancelling or waiting — see
	// the stopping field's doc. This must happen first so that any
	// processEntry call already past its own admitMu section (Add done)
	// is guaranteed visible to workerWg before we ever call Wait().
	p.admitMu.Lock()
	p.stopping = true
	p.admitMu.Unlock()

	// Step 1: Cancel the main pump context first to stop new lease/dispatch.
	// This ensures tick() stops accepting new work BEFORE we wait for workers.
	p.cancel()

	// Step 2: Wait for all in-flight workers AND the main run() loop with a
	// unified 5-second grace period. We use a single deadline for both to
	// keep the total shutdown time bounded to 5s, not 10s (P1-1). Without this,
	// a hung tick() DB call (stuck disk/filesystem) would make Stop() hang
	// forever, breaking the "bounded shutdown" guarantee in App.Shutdown.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	workerDone := make(chan struct{})
	go func() {
		p.workerWg.Wait()
		close(workerDone)
	}()

	mainLoopDone := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(mainLoopDone)
	}()

	select {
	case <-workerDone:
		// Workers finished. Now wait for the main run() loop to exit.
		select {
		case <-mainLoopDone:
			p.started = false
			slog.Info("run_queue_pump: stopped gracefully", "instance_id", p.cfg.PumpInstanceID)
			return false
		case <-shutdownCtx.Done():
			// Main loop didn't finish in time - forced shutdown
			slog.Warn("run_queue_pump: forced shutdown (main loop still running after 5s grace)", "instance_id", p.cfg.PumpInstanceID)
			return true
		}
	case <-mainLoopDone:
		// Main loop finished. Now wait for workers.
		select {
		case <-workerDone:
			p.started = false
			slog.Info("run_queue_pump: stopped gracefully", "instance_id", p.cfg.PumpInstanceID)
			return false
		case <-shutdownCtx.Done():
			// Workers didn't finish in time - forced shutdown
			slog.Warn("run_queue_pump: forced shutdown (workers still running after 5s grace)", "instance_id", p.cfg.PumpInstanceID)
			return true
		}
	case <-shutdownCtx.Done():
		// Neither workers nor main loop finished in time - forced shutdown
		slog.Warn("run_queue_pump: forced shutdown (workers and/or main loop still running after 5s grace)", "instance_id", p.cfg.PumpInstanceID)
		return true
	}
}

// run is the main pump loop.
func (p *RunQueuePump) run() {
	defer p.wg.Done()

	// Determine tick interval
	interval := RunQueuePumpInterval
	if p.cfg.TestTick != nil {
		interval = p.cfg.TestTick()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Determine drain interval
	drainInterval := OrphanOutboxDrainInterval
	if p.cfg.TestDrainTick != nil {
		drainInterval = p.cfg.TestDrainTick()
	}
	drainTicker := time.NewTicker(drainInterval)
	defer drainTicker.Stop()

	// Start orphan outbox drain goroutine. Runs an initial drain immediately
	// (P1-4 fix, docs/reviews/2026-08-13-release-readiness-static-audit.md
	// §P1-4), mirroring p.tick()'s own initial call below — otherwise the
	// first drain wouldn't happen until drainInterval (15s in production)
	// has elapsed, and a short-lived process (crush run) could start and
	// exit without ever attempting to recover a pending outbox entry.
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if p.ctx.Err() == nil {
			p.drainOrphanOutbox()
		}
		for {
			select {
			case <-p.ctx.Done():
				return
			case <-drainTicker.C:
				p.drainOrphanOutbox()
			}
		}
	}()

	// Initial tick on startup to recover any orphaned work
	p.tick()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.tick()
		}
	}
}

// tick performs one scan of the queue and attempts to execute pending work.
func (p *RunQueuePump) tick() {
	// Use a deadline-bound context for DB reads to prevent indefinite hangs
	// on stuck disk/filesystem (P1-1). The 5s budget matches Stop()'s total
	// grace period - if tick() itself hangs, Stop() will force-shutdown after
	// the same deadline anyway, so this protects both the shutdown path and
	// prevents a single stuck tick from blocking the pump indefinitely.
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Step 1: Cleanup expired leases (recovery from crashed pumps)
	expiredBefore := time.Now().Unix()
	if err := p.cfg.Sessions.CleanupExpiredLeases(ctx, expiredBefore); err != nil {
		slog.Warn("run_queue_pump: cleanup expired leases failed", "err", err, "instance_id", p.cfg.PumpInstanceID)
	}

	// Step 1.5: sweep expired busyBackoffUntil entries. processEntry's own
	// lazy delete (see there) only fires when a PENDING entry for that
	// session is actually rescanned — a session whose entry was acked,
	// terminal-failed, or picked up by a different pump instance in the
	// meantime never gets rescanned, so its key would otherwise linger in
	// this map for the rest of the process lifetime. Found by the fifth
	// @oh review pass over #337-349 (unbounded growth, not a correctness
	// bug — a stale key can only ever make this pump wait slightly longer
	// than necessary before trying a session again, never cause incorrect
	// behavior).
	now := time.Now()
	p.busyBackoffMu.Lock()
	for sessionID, until := range p.busyBackoffUntil {
		if !now.Before(until) {
			delete(p.busyBackoffUntil, sessionID)
		}
	}
	p.busyBackoffMu.Unlock()

	// Step 2: Scan for pending entries (and now-recovered stale leases)
	pending, err := p.cfg.Sessions.ListPendingRunQueueEntries(ctx)
	if err != nil {
		slog.Warn("run_queue_pump: list pending failed", "err", err, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	if len(pending) == 0 {
		return // No work to do
	}

	slog.Debug("run_queue_pump: found pending entries", "count", len(pending), "instance_id", p.cfg.PumpInstanceID)

	// Step 3: Attempt to lease and execute each pending entry
	for _, entry := range pending {
		if p.ctx.Err() != nil {
			return // Shutdown requested mid-tick
		}

		p.processEntry(ctx, &entry)
	}

	// Test seam notification
	select {
	case p.tickCh <- struct{}{}:
	default:
	}
}
