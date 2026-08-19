// Orphan outbox drain: the slow fallback path that moves pending orphan
// outbox entries into the main durable run queue, one atomic transaction
// per entry.

package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/db"
)

// drainOrphanOutbox performs one scan of the orphan outbox and attempts
// to move pending entries to the main run queue.
func (p *RunQueuePump) drainOrphanOutbox() {
	// Use a deadline-bound context for DB reads to prevent indefinite hangs
	ctx, cancel := context.WithTimeout(p.ctx, 5*time.Second)
	defer cancel()

	// Scan for pending orphan outbox entries
	pending, err := p.cfg.Sessions.ListPendingOrphanOutboxEntries(ctx)
	if err != nil {
		slog.Warn("run_queue_pump: drain orphan outbox failed to list pending", "err", err, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	if len(pending) == 0 {
		return // No orphan outbox work to do
	}

	slog.Info("run_queue_pump: draining orphan outbox entries", "count", len(pending), "instance_id", p.cfg.PumpInstanceID)

	// Attempt to move each pending entry to the main run queue
	for _, entry := range pending {
		if p.ctx.Err() != nil {
			return // Shutdown requested mid-drain
		}

		p.processOrphanOutboxEntry(ctx, entry)
	}
}

// processOrphanOutboxEntry attempts to move a single orphan
// outbox entry to the main run queue atomically.
func (p *RunQueuePump) processOrphanOutboxEntry(_ context.Context, entry db.OrphanCallOutbox) {
	// Derive from p.ctx, NOT context.Background() -- found by independent
	// review (docs/reviews/2026-08-13-release-readiness-static-audit.md,
	// finding M6): using context.Background() here detached this drain
	// entirely from p.ctx/Stop(), so a slow drain could keep running for
	// up to 10s past Stop() being called, well beyond Stop()'s own 5s
	// shutdown grace period -- which would then report a forced
	// (non-graceful) shutdown even though nothing was actually stuck, just
	// running on a clock nothing else knew about.
	//
	// Also NOT the caller's ctx parameter (drainOrphanOutbox's own 5s-
	// bound scan context, now unused here): chaining context.WithTimeout
	// off an already-5s-bound parent still caps the effective deadline at
	// 5s regardless of the timeout passed here (context.WithTimeout takes
	// the EARLIER of the two deadlines) -- that 5s budget is sized for the
	// list scan, not for this transaction, and cutting the transaction
	// short at that boundary would be worse than giving it a longer, but
	// still pump-shutdown-aware, budget. Deriving straight from p.ctx (not
	// context.Background(), not the 5s-capped scan ctx) gives both:
	// cancelled promptly on Stop(), with a real 10s budget in the
	// meantime.
	drainCtx, drainCancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer drainCancel()

	// Atomically drain the entry (INSERT to main queue + DELETE from orphan outbox in one tx)
	// This eliminates the vulnerable 'processing' intermediate state that could leave
	// entries stranded after crashes (P0-4 fix).
	//
	// The drain itself stays a single atomic transaction (insert-to-main-
	// queue + delete-from-outbox) with no intermediate claim state: the
	// older claim/mark-failed model (task #426) is gone and is not coming
	// back. What DID come back is the retry budget — dropping the claim
	// model also dropped attempts/terminal-failed, so an entry whose
	// call_data is genuinely malformed (the ONLY way the inner INSERT can
	// keep failing forever, since the FK ON DELETE CASCADE on session_id
	// already removes any entry whose session no longer exists) was logged
	// at ERROR and retried every 15 seconds for the life of the process.
	// Better than silent loss, but unbounded log and DB churn for a row
	// that can never succeed — see the 2026-08-18 release-readiness review.
	//
	// The budget is counted in a SEPARATE write on the failure path only
	// (recordOrphanOutboxDrainFailure below), which is what keeps the two
	// compatible: nothing owns the row, nothing waits on the counter, and a
	// crash between the two simply means the attempt was not counted.
	drained, err := p.cfg.Sessions.DrainOrphanOutboxEntry(drainCtx, entry.ID)
	if err != nil {
		slog.Error("run_queue_pump: failed to drain orphan outbox entry", "id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		p.recordOrphanOutboxDrainFailure(entry, err)
		return
	}
	if !drained {
		// Entry was already drained by another pump instance or not in pending state
		slog.Debug("run_queue_pump: orphan outbox entry already drained, skipping", "id", entry.ID, "instance_id", p.cfg.PumpInstanceID)
		return
	}

	slog.Info("run_queue_pump: successfully drained orphan outbox entry to main queue",
		"id", entry.ID,
		"session_id", entry.SessionID,
		"instance_id", p.cfg.PumpInstanceID)
}

// recordOrphanOutboxDrainFailure charges one failed drain attempt against an
// entry's retry budget and reports, loudly and once, when that budget runs
// out and the entry is quarantined.
//
// Deliberately best-effort. It runs on its own short-lived context rooted in
// context.Background() rather than p.ctx: the drain that just failed may have
// failed BECAUSE p.ctx was cancelled by Stop(), and in that case the counter
// write must still land — otherwise a shutdown racing a poison row would
// reset its progress toward quarantine on every restart, which is exactly the
// forever-retry this closes. The budget is small and monotonic, so counting
// one attempt during shutdown is harmless; failing to count one is not.
//
// If the counter write itself fails there is nothing further to do: the entry
// stays pending and the next tick tries again. That degrades to the old
// behaviour rather than losing the row.
func (p *RunQueuePump) recordOrphanOutboxDrainFailure(entry db.OrphanCallOutbox, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.dbWriteTimeout())
	defer cancel()

	outcome, err := p.cfg.Sessions.RecordOrphanOutboxFailure(ctx, entry.ID, cause.Error())
	if err != nil {
		slog.Warn("run_queue_pump: could not record an orphan outbox drain failure; the entry stays pending and will be retried",
			"id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		return
	}
	if outcome.AlreadyTerminal {
		return
	}
	if outcome.Quarantined {
		// ERROR, not WARN: this row's work is now durably parked and will
		// never be enqueued. It is still in the table, with last_error set,
		// for an operator to inspect -- but nothing will retry it.
		slog.Error("run_queue_pump: orphan outbox entry exhausted its retry budget and was quarantined; its call will NOT be enqueued",
			"id", entry.ID, "session_id", entry.SessionID,
			"attempts", outcome.Attempts, "max_attempts", outcome.MaxAttempts,
			"last_error", cause.Error(), "instance_id", p.cfg.PumpInstanceID)
		return
	}
	slog.Warn("run_queue_pump: orphan outbox drain failed, attempt counted",
		"id", entry.ID, "session_id", entry.SessionID,
		"attempts", outcome.Attempts, "max_attempts", outcome.MaxAttempts,
		"instance_id", p.cfg.PumpInstanceID)
}
