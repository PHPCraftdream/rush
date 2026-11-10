package session

import (
	"context"
	"errors"
	"time"
)

// ErrAgentFinalizationDriverUnavailable refuses an unproven child-driver rebuild.
var ErrAgentFinalizationDriverUnavailable = errors.New("finalization_child_driver_unavailable")

func recordFinalizationQueueFailure(ctx context.Context, entry *RunQueueEntry, runErr error) error {
	if entry == nil || entry.finalizationLease == nil || runErr == nil {
		return nil
	}
	proof := entry.finalizationLease
	code := "continuation_execution_failed"
	switch {
	case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded):
		return nil // Preserve cancellation; do not start additional stop I/O.
	case errors.Is(runErr, ErrCallQueuedNotExecuted):
		code = "continuation_busy_unknown"
	case errors.Is(runErr, ErrAgentFinalizationDriverUnavailable):
		code = "continuation_driver_unavailable"
	case errors.Is(runErr, ErrAgentFinalizationChanged):
		code = "continuation_consumed_unknown"
	}
	// Preserve the first precise failure, including after another lease finds
	// the consumed receipt. Never write through a replaced or expired lease.
	_, err := proof.writer.ExecContext(ctx, `UPDATE session_agent_finalization_continuations
		SET failure_code=CASE WHEN failure_code='' THEN ? ELSE failure_code END
		WHERE row_id=? AND lease_token=? AND (consumed=1 OR ?)
		AND EXISTS(SELECT 1 FROM session_run_queue q WHERE q.id=session_agent_finalization_continuations.row_id
		AND q.status='leased' AND q.leased_by=? AND q.leased_at=? AND q.attempts=? AND q.lease_expires_at>?)`,
		code, proof.rowID, proof.token, code == "continuation_driver_unavailable", proof.leasedBy, proof.leasedAt, proof.attempts, time.Now().Unix())
	return err
}
