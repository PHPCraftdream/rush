package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// ErrAgentFinalizationLeaseChanged permits only a fresh real lease to retry.
var ErrAgentFinalizationLeaseChanged = errors.New("finalization_queue_lease_changed")

func finalizationQueueLeaseChanged() error {
	return errors.Join(ErrAgentFinalizationChanged, ErrAgentFinalizationLeaseChanged)
}

type agentFinalizationQueueLease struct {
	writer                 *sql.DB
	rowID, leasedBy, token string
	leasedAt, attempts     int64
}

type agentFinalizationQueueLeaseKey struct{}

// Only the real lease transaction can issue this process-local proof.
func (s *service) bindFinalizationQueueLeaseTx(ctx context.Context, tx *sql.Tx, entry *RunQueueEntry) error {
	var data SessionAgentCallData
	if json.Unmarshal([]byte(entry.CallData), &data) != nil || data.FinalizationRequirement == nil {
		return nil
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalization_continuations WHERE row_id=?)`, entry.ID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if entry.LeasedBy == "" {
		return ErrAgentFinalizationChanged
	}
	token := uuid.NewString()
	result, err := tx.ExecContext(ctx, `UPDATE session_agent_finalization_continuations SET lease_token=? WHERE row_id=?`, token, entry.ID)
	if err != nil {
		return err
	}
	if err := batchRequireRow(result.RowsAffected()); err != nil {
		return err
	}
	entry.finalizationLease = &agentFinalizationQueueLease{
		writer: s.db, rowID: entry.ID, leasedBy: entry.LeasedBy, token: token,
		leasedAt: entry.LeasedAt, attempts: entry.Attempts,
	}
	return nil
}

func withFinalizationQueueLease(ctx context.Context, entry *RunQueueEntry) context.Context {
	if entry == nil || entry.finalizationLease == nil {
		return ctx
	}
	copy := *entry.finalizationLease
	return context.WithValue(ctx, agentFinalizationQueueLeaseKey{}, &copy)
}
