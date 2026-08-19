// Orphan call outbox (orphan_call_outbox, P0-3): durable fallback for
// calls that could not reach the main run queue, drained back into it
// atomically by whichever pump wins the race.

package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/google/uuid"
)

// WriteToOrphanOutbox durably records a call that could not be enqueued to the
// main run queue (P0-3 fix). This provides a minimal durability guarantee:
// even when the main run queue is unavailable, we have a durable record that
// can be recovered later. Returns error if even the outbox write fails.
func (s *service) WriteToOrphanOutbox(ctx context.Context, id, sessionID string, callData []byte) error {
	if id == "" {
		id = uuid.NewString()
	}
	now := time.Now().Unix()
	_, err := s.q.WriteToOrphanOutbox(ctx, db.WriteToOrphanOutboxParams{
		ID:        id,
		SessionID: sessionID,
		CallData:  string(callData),
		CreatedAt: now,
		UpdatedAt: now,
	})
	return err
}

// ListPendingOrphanOutboxEntries returns all pending outbox entries for
// recovery (scanned by pump).
func (s *service) ListPendingOrphanOutboxEntries(ctx context.Context) ([]db.OrphanCallOutbox, error) {
	return s.q.ListPendingOrphanOutboxEntries(ctx)
}

// GetOrphanOutboxEntry retrieves a single outbox entry by ID.
func (s *service) GetOrphanOutboxEntry(ctx context.Context, id string) (*db.OrphanCallOutbox, error) {
	entry, err := s.q.GetOrphanOutboxEntry(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &entry, err
}

// DrainOrphanOutboxEntry atomically moves an entry from orphan_call_outbox to
// session_run_queue in a single database transaction. This eliminates the
// vulnerable 'processing' intermediate state that could leave entries stranded
// after process crashes (P0-4 fix).
//
// The operation is idempotent and safe for concurrent pump instances:
// 1. Check if entry exists and is still pending
// 2. INSERT into session_run_queue (idempotent via ON CONFLICT DO NOTHING)
// 3. DELETE from orphan_call_outbox (only if status='pending')
// 4. Both operations happen in one transaction, so there's no window for crashes
//
// Returns true if the entry was successfully drained (moved to main queue),
// false if the entry was already drained by another pump or not in pending state.
func (s *service) DrainOrphanOutboxEntry(ctx context.Context, id string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	// First, check if the entry exists and is still pending
	entry, err := qtx.GetOrphanOutboxEntry(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Entry doesn't exist - already drained
			return false, nil
		}
		return false, err
	}

	if entry.Status != "pending" {
		// Entry is not pending - already claimed/processed by another pump
		return false, nil
	}

	now := time.Now().Unix()

	// Insert into session_run_queue (idempotent via ON CONFLICT DO NOTHING)
	_, err = qtx.EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{
		ID:        entry.ID,
		SessionID: entry.SessionID,
		CallData:  entry.CallData,
		CreatedAt: entry.CreatedAt,
		UpdatedAt: now,
	})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		// sql.ErrNoRows means it was already inserted (idempotent success)
		// Any other error is a real failure
		return false, fmt.Errorf("enqueueing to main queue: %w", err)
	}

	// Delete from orphan_call_outbox (only if still pending)
	rowsDeleted, err := qtx.DeleteOrphanOutboxEntryIfPending(ctx, entry.ID)
	if err != nil {
		return false, fmt.Errorf("deleting from orphan outbox: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing transaction: %w", err)
	}

	// Return whether we actually deleted a row (true = we drained it, false = another pump did)
	return rowsDeleted > 0, nil
}

// OrphanOutboxFailureOutcome reports what one failed drain attempt did to an
// entry's retry budget.
type OrphanOutboxFailureOutcome struct {
	// Attempts is the entry's failure count after this attempt.
	Attempts int64
	// MaxAttempts is the budget it is counted against.
	MaxAttempts int64
	// Quarantined is true when this attempt exhausted the budget and moved
	// the entry to the terminal 'failed' state, so it will no longer be
	// scanned by ListPendingOrphanOutboxEntries.
	Quarantined bool
	// AlreadyTerminal is true when the row was not in 'pending' state at
	// all -- someone else quarantined or drained it first. Nothing was
	// counted; there is nothing for the caller to do.
	AlreadyTerminal bool
}

// RecordOrphanOutboxFailure counts one failed drain attempt against an entry
// and quarantines it once it has used up max_attempts.
//
// Deliberately NOT part of DrainOrphanOutboxEntry's transaction. That
// transaction is an atomic insert-to-main-queue plus delete-from-outbox and
// must stay that way; this runs only after it has already failed and rolled
// back. The point of the 2026-08-18 review finding was that dropping the old
// claim/mark-failed model also dropped the retry budget, so a genuinely
// malformed row was logged and retried every 15 seconds forever. This
// restores the budget without restoring the claim.
//
// It takes no ownership: nothing waits on this write, and a crash between the
// failed drain and this call simply means the attempt was not counted.
func (s *service) RecordOrphanOutboxFailure(ctx context.Context, id, lastError string) (OrphanOutboxFailureOutcome, error) {
	row, err := s.q.RecordOrphanOutboxFailure(ctx, db.RecordOrphanOutboxFailureParams{
		ID:        id,
		LastError: sql.NullString{String: lastError, Valid: lastError != ""},
		UpdatedAt: time.Now().Unix(),
	})
	if errors.Is(err, sql.ErrNoRows) {
		// The UPDATE is scoped to status = 'pending', so no rows means the
		// entry was drained or quarantined by someone else in the meantime.
		// Not an error: there is simply nothing left to count.
		return OrphanOutboxFailureOutcome{AlreadyTerminal: true}, nil
	}
	if err != nil {
		return OrphanOutboxFailureOutcome{}, err
	}
	return OrphanOutboxFailureOutcome{
		Attempts:    row.Attempts,
		MaxAttempts: row.MaxAttempts,
		Quarantined: row.Status != "pending",
	}, nil
}
