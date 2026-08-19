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
	// back. What DID come back is the retry budget -- dropping the claim
	// model also dropped attempts/terminal-failed, so an entry whose
	// call_data is genuinely malformed (the ONLY way the inner INSERT can
	// keep failing forever, since the FK ON DELETE CASCADE on session_id
	// already removes any entry whose session no longer exists) was logged
	// at ERROR and retried every 15 seconds for the life of the process.
	// Better than silent loss, but unbounded log and DB churn for a row
	// that can never succeed -- see the 2026-08-18 release-readiness review.
	//
	// The budget is counted in a SEPARATE write on the failure path only
	// (recordOrphanOutboxDrainFailure below), which is what keeps the two
	// compatible: nothing owns the row, nothing waits on the counter, and a
	// crash between the two simply means the attempt was not counted.
	drained, err := p.cfg.Sessions.DrainOrphanOutboxEntry(drainCtx, entry.ID)
	if err != nil {
		slog.Error("run_queue_pump: failed to drain orphan outbox entry", "id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
		// P1 fix (task #589 of the 2026-08-19 release-readiness
		// static-follow-up review, which corrected #571 part c's polarity
		// after finding it still counted routine shutdown as permanent): a
		// cancellation caused by p.ctx itself -- an ordinary Stop() landing
		// mid-transaction, or this call's own 10s drainCtx deadline (which
		// is derived from p.ctx, see drainCtx above) expiring on a slow
		// disk -- must not be charged against max_attempts AT ALL, before
		// the transient/permanent classifier below even runs. It says
		// nothing about this row's data and nothing about the database
		// being unhealthy; it is purely "the process is shutting down" or
		// "this one attempt happened to run long". Checking p.ctx.Err()
		// specifically (not drainCtx.Err(), and not by inspecting err's
		// type) is what makes this precise: p.ctx is only ever cancelled by
		// Stop() (see RunQueuePump.Stop), so a non-nil p.ctx.Err() here can
		// only mean "shutdown is why this attempt did not finish", never
		// "the database rejected this row's data".
		if p.ctx.Err() != nil {
			slog.Warn("run_queue_pump: orphan outbox drain aborted by shutdown, not counted against the retry budget",
				"id", entry.ID, "session_id", entry.SessionID, "err", err, "instance_id", p.cfg.PumpInstanceID)
			return
		}
		// The classifier below is inverted from the #571 version: instead
		// of a narrow transient allow-list (SQLITE_BUSY/SQLITE_LOCKED) with
		// everything else -- including a context deadline exceeded on this
		// call's own budget, SQLITE_IOERR/FULL/a transient READONLY, and
		// any error type this function has never seen -- defaulting to
		// "permanent", it now POSITIVELY recognises the one cause that is
		// actually proven permanent (a constraint/FK violation on the row's
		// OWN data, see isPermanentOrphanOutboxDrainError's doc) and treats
		// every other outcome, known-retryable or merely unrecognised
		// alike, as transient: logged loudly (the slog.Error above already
		// fired) but left pending for the next tick rather than spent from
		// a budget that exists for a different, positively-diagnosed
		// failure mode. A quarantine that cannot tell "this row is broken"
		// from "something unexpected happened" has no business being the
		// one that spends the budget.
		if !isPermanentOrphanOutboxDrainError(err) {
			slog.Warn("run_queue_pump: orphan outbox drain failed without a positively-recognised permanent cause, not counted against the retry budget",
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
// write must still land -- otherwise a shutdown racing a poison row would
// reset its progress toward quarantine on every restart, which is exactly the
// forever-retry this closes. The budget is small and monotonic, so counting
// one attempt during shutdown is harmless; failing to count one is not.
//
// Only ever reached for a positively-recognised permanent cause (see
// isPermanentOrphanOutboxDrainError) -- a p.ctx cancellation never reaches
// here at all (processOrphanOutboxEntry returns before calling this), so
// "shutdown racing a poison row" above means the SAME poison row failing
// again on a genuinely permanent (constraint) error while a shutdown is
// ALSO in flight for an unrelated reason, not the shutdown itself being
// counted.
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

// isPermanentOrphanOutboxDrainError classifies a DrainOrphanOutboxEntry
// failure as PERMANENT (this exact row, with this exact data, cannot ever
// succeed, no matter how many times it is retried) versus everything else,
// which is treated as transient/unknown and left pending rather than
// counted against the retry budget.
//
// This is the inverse polarity of the classifier's original (#571) shape,
// corrected by the 2026-08-19 static-follow-up review (task #589): the
// original function was named isTransientOrphanOutboxDrainError and
// positively recognised only SQLITE_BUSY/SQLITE_LOCKED as transient, with
// every other outcome -- including ctx.DeadlineExceeded/ctx.Canceled from
// this call's OWN budget, SQLITE_IOERR, SQLITE_FULL, a transient
// SQLITE_READONLY, an FS/network hiccup wrapped some other way, and any
// error type this function has never seen -- silently defaulting to
// "permanent" and spending the budget. Its own doc called that the "safe"
// direction; it is not. A quarantine's job is to durably stop retrying work
// that is PROVEN unrecoverable. A default-permanent classifier instead
// quarantines on ignorance: five ordinary shutdown races, disk hiccups, or
// future error shapes this function has never seen permanently strand
// otherwise-healthy, already-accepted user work with the row moved to
// 'failed' and never scanned again -- worse than the unbounded retry the
// quarantine was built to replace.
//
// The correct polarity for a quarantine is to positively recognise the one
// cause that IS proven permanent and default everything else to "keep
// retrying, but say so loudly" (the slog.Error at the DrainOrphanOutboxEntry
// call site already fires unconditionally before this classifier runs, so
// an unrecognised failure is never silent -- it is diagnosed and visible,
// just not spent from a budget it may not deserve to be spent from).
//
// What IS positively permanent: DrainOrphanOutboxEntry's own doc identifies
// exactly one reachable this-row-can-never-succeed failure -- the
// EnqueueRunQueueEntry insert failing session_run_queue's
// "session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE"
// foreign key (see that function's doc for how such a row can exist despite
// orphan_call_outbox sharing the identical FK+CASCADE, and
// p556_orphan_outbox_quarantine_test.go for how it is constructed in
// tests). That surfaces as a genuine *sqlite.Error with primary result code
// SQLITE_CONSTRAINT (19) -- retrying changes nothing because the row's own
// session_id will never start existing in the sessions table on its own.
// Nothing else DrainOrphanOutboxEntry's transaction can fail on (BeginTx,
// GetOrphanOutboxEntry, Commit) is a proven permanent condition -- each is
// disk/lock/resource-shaped, which is exactly the kind of failure that can
// clear on a later tick.
//
// Classification is by SQLite result code, not by pattern-matching the
// error string, for the same reason the original version gave: driver
// wording is not a stable contract. Code() returns SQLite's raw result
// code, which under default builds is the EXTENDED code (e.g. 787 for
// SQLITE_CONSTRAINT_FOREIGNKEY, not the primary 19) -- masking with &0xff
// recovers the primary code this comparison is written against, per
// SQLite's own documented layout (see sqlite.org/rescode.html: "the
// primary result code ... corresponds to the least significant 8 bits").
//
// A non-*sqlite.Error (including every context error: ctx.DeadlineExceeded
// on this call's own 10s budget, or a p.ctx cancellation that -- for
// defense in depth -- processOrphanOutboxEntry ALSO checks and short-
// circuits on before this function is ever called) returns false: not
// permanent, so not counted. Any future change tightening this list must
// keep that asymmetry: a type or code this function has never seen must
// stay in the "retry, don't quarantine" bucket, not fall through to
// "permanent" by default.
func isPermanentOrphanOutboxDrainError(err error) bool {
	var sqliteErr *sqlitedriver.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case 19: // SQLITE_CONSTRAINT (covers the FK-violation extended codes too, masked with &0xff)
		return true
	default:
		return false
	}
}
