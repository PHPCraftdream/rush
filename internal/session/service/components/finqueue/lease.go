package finqueue

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Lease is process-local proof that a specific queue lease owns a continuation.
type Lease struct {
	writer                 *sql.DB
	rowID, leasedBy, Token string
	leasedAt, attempts     int64
}

type LeaseContextKey struct{}

func (l *Lease) Writer() *sql.DB  { return l.writer }
func (l *Lease) RowID() string    { return l.rowID }
func (l *Lease) LeasedBy() string { return l.leasedBy }
func (l *Lease) LeasedAt() int64  { return l.leasedAt }
func (l *Lease) Attempts() int64  { return l.attempts }

// BindQueueLeaseTx binds the continuation token in the queue lease transaction.
func BindQueueLeaseTx(ctx context.Context, writer *sql.DB, tx *sql.Tx, rowID, leasedBy string, leasedAt, attempts int64, changed func() error, requireRow func(int64, error) error) (*Lease, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalization_continuations WHERE row_id=?)`, rowID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	if leasedBy == "" {
		return nil, changed()
	}
	token := uuid.NewString()
	result, err := tx.ExecContext(ctx, `UPDATE session_agent_finalization_continuations SET lease_token=? WHERE row_id=?`, token, rowID)
	if err != nil {
		return nil, err
	}
	if err := requireRow(result.RowsAffected()); err != nil {
		return nil, err
	}
	return &Lease{writer: writer, rowID: rowID, leasedBy: leasedBy, Token: token, leasedAt: leasedAt, attempts: attempts}, nil
}

// WithQueueLease adds a detached lease proof to ctx.
func WithQueueLease(ctx context.Context, lease *Lease) context.Context {
	if lease == nil {
		return ctx
	}
	copy := *lease
	return context.WithValue(ctx, LeaseContextKey{}, &copy)
}

// RecordQueueFailure records only failures still owned by the exact live lease.
func RecordQueueFailure(ctx context.Context, proof *Lease, runErr error, busy, unavailable, changed error) error {
	if proof == nil || runErr == nil {
		return nil
	}
	code := "continuation_execution_failed"
	switch {
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		return nil
	case errors.Is(runErr, busy):
		code = "continuation_busy_unknown"
	case errors.Is(runErr, unavailable):
		code = "continuation_driver_unavailable"
	case errors.Is(runErr, changed):
		code = "continuation_consumed_unknown"
	}
	_, err := proof.writer.ExecContext(ctx, `UPDATE session_agent_finalization_continuations
		SET failure_code=CASE WHEN failure_code='' THEN ? ELSE failure_code END
		WHERE row_id=? AND lease_token=? AND (consumed=1 OR ?)
		AND EXISTS(SELECT 1 FROM session_run_queue q WHERE q.id=session_agent_finalization_continuations.row_id
		AND q.status='leased' AND q.leased_by=? AND q.leased_at=? AND q.attempts=? AND q.lease_expires_at>?)`,
		code, proof.rowID, proof.Token, code == "continuation_driver_unavailable", proof.leasedBy, proof.leasedAt, proof.attempts, time.Now().Unix())
	return err
}
