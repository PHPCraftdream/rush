// Cross-process message inject queue (pending_injects): plain merge
// injects drained at PrepareStep, and interrupt-style injects owned by
// the interrupt ticker via its peek/consume/delete protocol.

package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
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
// The caller (e.g. `rush sessions inject`) is responsible for having
// already created the referenced messages row so it is immediately visible
// in the web UI; this only records the request to splice it into the live
// prompt of whatever process is running the session.
func (s *service) CreatePendingInject(ctx context.Context, inject PendingInject) (retErr error) {
	if inject.ID == "" {
		inject.ID = uuid.NewString()
	}
	if inject.CreatedAt == 0 {
		inject.CreatedAt = time.Now().Unix()
	}
	if finalizationScoped(ctx) {
		// A duplicate id is not productive work: refuse before admission can
		// mint a revision. Unscoped callers keep the writer's own error.
		var present bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_injects WHERE id=?)`, inject.ID).Scan(&present); err != nil {
			return err
		}
		if present {
			return fmt.Errorf("pending inject %q already exists", inject.ID)
		}
	}
	release, admission, err := admitFinalizationMutation(ctx, s.db, inject.SessionID)
	if release != nil {
		defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := validateBatchFinalizationTx(ctx, tx, []string{inject.SessionID}, admission); err != nil {
		return err
	}
	interrupt := int64(0)
	if inject.Interrupt {
		interrupt = 1
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		inject.ID, inject.SessionID, inject.MessageID, inject.Content, interrupt, inject.CreatedAt,
	)
	if err != nil {
		return err
	}
	if err := validateBatchFinalizationTx(ctx, tx, []string{inject.SessionID}, admission); err != nil {
		return err
	}
	return tx.Commit()
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
func (s *service) DrainPendingInjects(ctx context.Context, sessionID string) (merge []PendingInject, hasInterrupt bool, retErr error) {
	var admission *batchFinalizationAdmission
	if finalizationScoped(ctx) {
		// An empty drain is not productive work and must not mint a revision.
		var productive bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_injects WHERE session_id=? AND interrupt=0)`, sessionID).Scan(&productive); err != nil {
			return nil, false, err
		}
		if productive {
			var release func() error
			var err error
			release, admission, err = admitFinalizationMutation(ctx, s.db, sessionID)
			if release != nil {
				defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
			}
			if err != nil {
				return nil, false, err
			}
		}
	}
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

	var consumedIDs []string
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

	if len(consumedIDs) > 0 {
		if err := validateBatchFinalizationTx(ctx, tx, []string{sessionID}, admission); err != nil {
			return nil, false, err
		}
	}
	for _, id := range consumedIDs {
		if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, id); delErr != nil {
			return nil, false, delErr
		}
	}
	if len(consumedIDs) > 0 {
		if err := validateBatchFinalizationTx(ctx, tx, []string{sessionID}, admission); err != nil {
			return nil, false, err
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
// Uses the standalone WAL reader so a busy writer cannot time out CLI waiting.
// Returns (nil, nil) when no committed interrupt row is pending.
func (s *service) PeekInterruptInject(ctx context.Context, sessionID string) (*PendingInject, error) {
	var (
		pi        PendingInject
		interrupt int64
	)
	row := s.readDB.QueryRowContext(ctx,
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
// they have confirmed execution (acquired OS lock). P0-2 fix. It returns
// sql.ErrNoRows when another consumer already removed the row.
func (s *service) DeleteInterruptInject(ctx context.Context, injectID string) (retErr error) {
	var scopedOwner string
	var admission *batchFinalizationAdmission
	if finalizationScoped(ctx) {
		// A missing row is not productive work: no admission, legacy result.
		if err := s.db.QueryRowContext(ctx, `SELECT session_id FROM pending_injects WHERE id=?`, injectID).Scan(&scopedOwner); err != nil {
			return err
		}
		var release func() error
		var err error
		release, admission, err = admitFinalizationMutation(ctx, s.db, scopedOwner)
		if release != nil {
			defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
		}
		if err != nil {
			return err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var owner string
	if err := tx.QueryRowContext(ctx, `SELECT session_id FROM pending_injects WHERE id = ?`, injectID).Scan(&owner); err != nil {
		return err
	}
	if scopedOwner != "" && owner != scopedOwner {
		return ErrAgentFinalizationChanged
	}
	if err := validateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return err
	}

	result, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, injectID)
	if delErr != nil {
		return delErr
	}
	rows, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return rowsErr
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	if err := validateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return err
	}
	return tx.Commit()
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
func (s *service) ConsumeInterruptInjectAndEnqueue(ctx context.Context, sessionID, injectID, idempotencyKey string, callData []byte) (ret *PendingInject, retErr error) {
	if finalizationScoped(ctx) {
		// A vanished source row is not productive work: no admission.
		var present bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pending_injects WHERE id=? AND session_id=? AND interrupt=1)`, injectID, sessionID).Scan(&present); err != nil {
			return nil, err
		}
		if !present {
			return nil, nil
		}
	}
	payload, err := s.prepareFinalizationIngress(ctx, sessionID, callData)
	if err != nil {
		return nil, err
	}
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	release, admission, err := admitFinalizationMutation(ctx, s.db, sessionID)
	if release != nil {
		defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return nil, err
	}
	ancestryHash, err := finalizationIngressAncestryHash(payload, admission)
	if err != nil {
		return nil, err
	}
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
	if err := validateBatchFinalizationTx(ctx, tx, []string{sessionID}, admission); err != nil {
		return nil, err
	}

	// Delete the pending inject row first
	if _, delErr := tx.ExecContext(ctx, `DELETE FROM pending_injects WHERE id = ?`, pi.ID); delErr != nil {
		return nil, delErr
	}

	// Then enqueue to run queue in the same transaction
	if existing, err := verifyInterruptEnqueueCollisionTx(ctx, tx, idempotencyKey, sessionID, payload); err != nil {
		return nil, err
	} else if !existing {
		if err := validateFreshFinalizationIngressTx(ctx, tx, sessionID, idempotencyKey, payload, ancestryHash, admission); err != nil {
			return nil, err
		}
		now := time.Now().Unix()
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
		if err := persistFinalizationIngressTx(ctx, tx, idempotencyKey, sessionID, payload, ancestryHash); err != nil {
			return nil, err
		}
	}
	if err := validateBatchFinalizationTx(ctx, tx, []string{sessionID}, admission); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &pi, nil
}

// verifyInterruptEnqueueCollisionTx handles an idempotency hit. A bound
// replacement must match its recorded provenance; an unbound hit keeps the
// historical success and only refuses a newly bound owner scope.
func verifyInterruptEnqueueCollisionTx(ctx context.Context, tx *sql.Tx, key, owner string, payload finalizationIngressPayload) (bool, error) {
	_, existingRaw, exists, err := finalizationQueueRowTx(ctx, tx, key)
	if err != nil || !exists {
		return false, err
	}
	if payload.bound {
		return verifyQueueCollisionTx(ctx, tx, key, owner, payload)
	}
	if _, err := validateFinalizationIngressTx(ctx, tx, owner, key, existingRaw, payload); err != nil {
		return false, err
	}
	return true, nil
}

// ReconcileInterruptInjectEnqueue removes the exact durable replacement made
// for inject and restores inject in one transaction. idempotencyKey identifies
// one attempt, not the source inject. A missing replacement is treated as an
// already-completed reconciliation, which makes retry after an ambiguous
// commit safe without deleting another attempt's row or restoring its source.
func (s *service) ReconcileInterruptInjectEnqueue(ctx context.Context, inject PendingInject, idempotencyKey string) (retErr error) {
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT call_data FROM session_run_queue WHERE id = ? AND session_id = ?`,
		idempotencyKey, inject.SessionID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	payload, err := s.prepareFinalizationIngress(ctx, inject.SessionID, []byte(raw))
	if err != nil {
		return err
	}
	release, admission, err := admitFinalizationMutation(ctx, s.db, inject.SessionID)
	if release != nil {
		defer func() { retErr = batchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var actualRaw string
	if err := tx.QueryRowContext(ctx, `SELECT call_data FROM session_run_queue WHERE id = ? AND session_id = ?`,
		idempotencyKey, inject.SessionID).Scan(&actualRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if payload.bound && actualRaw != raw {
		return ErrAgentFinalizationChanged
	}
	if err := validateBatchFinalizationTx(ctx, tx, []string{inject.SessionID}, admission); err != nil {
		return err
	}
	if _, err := validateFinalizationIngressTx(ctx, tx, inject.SessionID, idempotencyKey, raw, payload); err != nil {
		return err
	}

	result, err := tx.ExecContext(ctx,
		`DELETE FROM session_run_queue WHERE id = ? AND session_id = ?`,
		idempotencyKey, inject.SessionID,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil
	}
	if payload.bound {
		// The replacement is gone, so its unconsumed provenance goes with it.
		cleared, err := tx.ExecContext(ctx, `DELETE FROM session_agent_finalization_continuations WHERE row_id = ? AND consumed = 0`, idempotencyKey)
		if err != nil {
			return err
		}
		if n, err := cleared.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrAgentFinalizationChanged
		}
	}

	interrupt := int64(0)
	if inject.Interrupt {
		interrupt = 1
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO pending_injects (id, session_id, message_id, content, interrupt, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		inject.ID, inject.SessionID, inject.MessageID, inject.Content, interrupt, inject.CreatedAt,
	)
	if err != nil {
		return err
	}
	if err := validateBatchFinalizationTx(ctx, tx, []string{inject.SessionID}, admission); err != nil {
		return err
	}
	return tx.Commit()
}
