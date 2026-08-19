// Orphan outbox drain: the slow fallback path that moves pending orphan
// outbox entries into the main durable run queue, one atomic transaction
// per entry.

package session

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	sqlitedriver "modernc.org/sqlite"
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
		// P1 fix (task #571 of the 2026-08-19 release-readiness review): a
		// TRANSIENT failure (DB lock contention -- SQLITE_BUSY/SQLITE_LOCKED,
		// the concrete shape being a losing writer in SQLite's single-writer
		// model, exactly as multiple pump instances each running an
		// immediate Start()-time drain would produce) must NOT be charged
		// against max_attempts. That budget exists for a row that can NEVER
		// succeed no matter how many times it is retried -- a session_id
		// that fails session_run_queue's FK check being the concrete,
		// reachable shape (see DrainOrphanOutboxEntry's own doc for why a
		// normally FK-CASCADE-deleted session cannot reach this path, and
		// isTransientOrphanOutboxDrainError's doc for how a row like that
		// gets here anyway). A transient DB error says nothing about the
		// row's data; the row is exactly as healthy after it as before.
		// Charging it anyway means enough routine contention -- five ticks'
		// worth, no backoff between them -- permanently quarantines a
		// perfectly good, still-queued user call with nothing left to
		// recover it.
		if isTransientOrphanOutboxDrainError(err) {
			slog.Warn("run_queue_pump: orphan outbox drain hit transient DB contention, not counted against the retry budget",
				"id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
			return
		}
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

// isTransientOrphanOutboxDrainError classifies a DrainOrphanOutboxEntry
// failure as transient (says nothing about the row's own data, will very
// likely clear on its own on a later tick) versus permanent (this exact
// row, with this exact data, cannot ever succeed, no matter how many times
// it is retried).
//
// Classification is by SQLite result code, not by pattern-matching the
// error string: DrainOrphanOutboxEntry's only failure paths are (1) the
// initial BeginTx, (2) the GetOrphanOutboxEntry read inside that
// transaction, (3) the EnqueueRunQueueEntry insert (the one genuinely
// permanent case -- a session_id that fails session_run_queue's FK check,
// see that function's own doc for how such a row can exist despite
// orphan_call_outbox sharing the identical FK+CASCADE), and (4) the final
// Commit -- and every one of those is a real database/sql call against
// modernc.org/sqlite, which wraps every SQLite-level failure in *sqlite.Error
// with a concrete numeric Code(). That code is what SQLite itself uses to
// distinguish "try again, nothing is wrong with your data" (SQLITE_BUSY,
// SQLITE_LOCKED) from "this exact statement can never succeed with this
// exact data" (SQLITE_CONSTRAINT and everything else) -- matching on it
// directly is precise where matching on err.Error()'s text would not be:
// driver wording is not a stable contract, and a string match would either
// miss a real code (silently falling back to "permanent", which is the
// SAFE direction to be wrong in) or accidentally match unrelated text (the
// UNSAFE direction, since it could exempt a genuinely permanent failure
// from ever being quarantined).
//
// Code() returns SQLite's raw result code, which under default builds is
// the EXTENDED code (e.g. 787 for SQLITE_CONSTRAINT_FOREIGNKEY, not the
// primary 19) -- masking with &0xff recovers the primary code these
// comparisons are written against, per SQLite's own documented layout
// (https://www.sqlite.org/rescode.html: "the primary result code ...
// corresponds to the least significant 8 bits").
//
// What this catches: SQLITE_BUSY (the writer lost SQLite's single-writer
// lock to a concurrent connection -- multiple pump instances, each running
// an immediate Start()-time drain, is the concrete production shape named
// in the 2026-08-19 release-readiness review) and SQLITE_LOCKED (a
// same-connection/shared-cache lock conflict; rarer with this driver's
// default connection setup, included because it is the other lock-status
// code SQLite defines alongside SQLITE_BUSY and carries the identical
// "retry later, nothing wrong with the data" meaning).
//
// What this deliberately does NOT catch, and why that is the safe
// direction: ctx.DeadlineExceeded/ctx.Canceled (drainCtx's own 10s budget,
// or Stop() during shutdown) are NOT classified as transient here, even
// though a slow-but-otherwise-healthy DB could in principle produce them --
// they are conflated with "no error information at all" instead
// (err.(*sqlite.Error) fails the type assertion for a context error, so
// this returns false, and the failure counts toward the budget exactly as
// it did before this fix). A context deadline during a single row's own
// 10s transaction window is unusual enough, and a false "permanent" classification
// merely costs one MORE point off a five-attempt budget rather than losing the
// row (it is still retried on the next tick either way, and the row is only ever
// quarantined after genuinely repeated failures) -- unlike the false
// "transient" direction, which would let a genuinely permanent failure (a
// bug in this classifier, or a future non-SQLite Sessions implementation
// wrapping errors in a type this function has never seen) retry forever,
// which is the exact defect task #440/#556 already closed once. A future
// change wiring ctx errors through explicitly would need to keep that
// asymmetry: err.(*sqlite.Error) failing to match must never itself be
// read as "retry forever".
func isTransientOrphanOutboxDrainError(err error) bool {
	var sqliteErr *sqlitedriver.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 5, // SQLITE_BUSY
		6: // SQLITE_LOCKED
		return true
	default:
		return false
	}
}
