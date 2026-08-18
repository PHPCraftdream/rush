// Cross-process message inject queue (pending_injects): plain merge
// injects drained at PrepareStep, and interrupt-style injects owned by
// the interrupt ticker via its peek/consume/delete protocol.

package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/google/uuid"
)

// PendingInject is one row of the cross-process inject queue. It is a
// signal pointing at an already-created messages row (MessageID); Content is
// carried only for debugging/logging. Interrupt distinguishes a plain merge
// (false) from an interrupt-style inject (true) owned by the interrupt
// ticker.
type PendingInject struct {
	ID        string
	SessionID string
	MessageID string
	Content   string
	Interrupt bool
	CreatedAt int64
}

// CreatePendingInject enqueues a cross-process inject signal for sessionID.
// The caller (e.g. `crush sessions inject`) is responsible for having
// already created the referenced messages row so it is immediately visible
// in the web UI; this only records the request to splice it into the live
// prompt of whatever process is running the session.
func (s *service) CreatePendingInject(ctx context.Context, inject PendingInject) error {
	if inject.ID == "" {
		inject.ID = uuid.NewString()
	}
	if inject.CreatedAt == 0 {
		inject.CreatedAt = time.Now().Unix()
	}
	interrupt := int64(0)
	if inject.Interrupt {
		interrupt = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		inject.ID, inject.SessionID, inject.MessageID, inject.Content, interrupt, inject.CreatedAt,
	)
	return err
}

// DrainPendingInjects consumes the non-interrupt (interrupt = 0) inject rows
// for sessionID, deleting them in the same transaction (delete-after-read),
// and returns them ordered oldest-first for merging into the current prompt.
// The second return value reports whether an interrupt (interrupt = 1) row is
// also pending; those rows are NOT returned or deleted here — they are owned
// by the interrupt ticker, which is expected to consume them before the next
// PrepareStep. Reporting their presence lets PrepareStep log a defensive
// warning if one slipped through.
//
// SQLite serialises writers, so there is no cross-process race; the enclosing
// transaction guards against two goroutines inside this process draining the
// same rows concurrently.
func (s *service) DrainPendingInjects(ctx context.Context, sessionID string) ([]PendingInject, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx,
		`SELECT id, session_id, message_id, content, interrupt, created_at
		 FROM pending_injects WHERE session_id = ? ORDER BY created_at ASC, rowid ASC`,
		sessionID,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var (
		merge        []PendingInject
		consumedIDs  []string
		hasInterrupt bool
	)
	for rows.Next() {
		var (
			pi        PendingInject
			interrupt int64
		)
		if scanErr := rows.Scan(&pi.ID, &pi.SessionID, &pi.MessageID, &pi.Content, &interrupt, &pi.CreatedAt); scanErr != nil {
			return nil, false, scanErr
		}
		if interrupt != 0 {
			pi.Interrupt = true
			hasInterrupt = true
			continue
		}
		merge = append(merge, pi)
		consumedIDs = append(consumedIDs, pi.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	for _, id := range consumedIDs {
		if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, id); delErr != nil {
			return nil, false, delErr
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return merge, hasInterrupt, nil
}

// PeekInterruptInject reads the OLDEST interrupt=true pending_injects row
// for sessionID WITHOUT deleting it. Used by handleInterruptTick to read
// the message reference before building call data, so it can call
// ConsumeInterruptInjectAndEnqueue atomically.
//
// Returns (nil, nil) when no interrupt row is pending.
func (s *service) PeekInterruptInject(ctx context.Context, sessionID string) (*PendingInject, error) {
	var (
		pi        PendingInject
		interrupt int64
	)
	row := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, message_id, content, interrupt, created_at
		 FROM pending_injects
		 WHERE session_id = ? AND interrupt = 1
		 ORDER BY created_at ASC, rowid ASC LIMIT 1`,
		sessionID,
	)
	scanErr := row.Scan(&pi.ID, &pi.SessionID, &pi.MessageID, &pi.Content, &interrupt, &pi.CreatedAt)
	if scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, scanErr
	}
	pi.Interrupt = interrupt != 0
	return &pi, nil
}

// DeleteInterruptInject removes a specific pending inject row by ID.
// Used by detached interrupt runs to delete the durable pending row AFTER
// they have confirmed execution (acquired OS lock). P0-2 fix.
func (s *service) DeleteInterruptInject(ctx context.Context, injectID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, injectID); delErr != nil {
		return delErr
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

// ConsumeInterruptInjectAndEnqueue atomically consumes the pending inject row
// identified by injectID and enqueues it to the run queue in a single
// transaction.
//
// injectID must be the ID of a row the caller already read (e.g. via
// PeekInterruptInject) and built callData from. Matching on that specific ID
// — rather than re-selecting "the oldest pending row" — matters because a
// session can have more than one interrupt=1 row queued: if some other
// path (a concurrent tick, a foreign process) deletes the peeked row between
// Peek and this call, re-selecting "oldest" would silently consume a
// DIFFERENT row than the one callData was built from, losing that row's
// content while the peeked row's own deletion goes unnoticed.
//
// Returns (nil, nil) when injectID no longer refers to a pending row (it
// vanished between peek and consume — e.g. already handled by another
// process); the caller should treat this as a no-op, not an error.
// Returns (pi, nil) when the row was successfully consumed and enqueued.
// Returns error on failure — the transaction is rolled back, so the row
// remains for retry.
func (s *service) ConsumeInterruptInjectAndEnqueue(ctx context.Context, sessionID, injectID, idempotencyKey string, callData []byte) (*PendingInject, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var (
		pi        PendingInject
		interrupt int64
	)
	row := tx.QueryRowContext(ctx,
		`SELECT id, session_id, message_id, content, interrupt, created_at
		 FROM pending_injects
		 WHERE id = ? AND session_id = ? AND interrupt = 1`,
		injectID, sessionID,
	)
	if scanErr := row.Scan(&pi.ID, &pi.SessionID, &pi.MessageID, &pi.Content, &interrupt, &pi.CreatedAt); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, scanErr
	}
	pi.Interrupt = interrupt != 0

	// Delete the pending inject row first
	if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, pi.ID); delErr != nil {
		return nil, delErr
	}

	// Then enqueue to run queue in the same transaction
	now := time.Now().Unix()
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	qtx := s.q.WithTx(tx)
	_, enqueueErr := qtx.EnqueueRunQueueEntry(ctx, db.EnqueueRunQueueEntryParams{
		ID:        idempotencyKey,
		SessionID: sessionID,
		CallData:  string(callData),
		CreatedAt: now,
		UpdatedAt: now,
	})
	if errors.Is(enqueueErr, sql.ErrNoRows) {
		// Idempotency hit: entry already exists with this key, treat as success
		enqueueErr = nil
	}
	if enqueueErr != nil {
		return nil, enqueueErr
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &pi, nil
}
