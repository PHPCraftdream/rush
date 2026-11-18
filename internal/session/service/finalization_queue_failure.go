package session

import (
	"context"
	"errors"
	"time"
)

// ErrCallQueuedNotExecuted is the pump sentinel for a call that was queued
// into an already-owned session without executing. (Staged here from
// run_queue_pump.go; see MV1 report, deviation (c), resolving lane EX1
// sesstypes/types_runcall.)
var ErrCallQueuedNotExecuted = errors.New("run_queue_pump: call was queued into an already-owned session, not executed")

// ErrAgentFinalizationDriverUnavailable refuses an unproven child-driver rebuild.
var ErrAgentFinalizationDriverUnavailable = errors.New("finalization_child_driver_unavailable")

func RecordFinalizationQueueFailure(ctx context.Context, entry *RunQueueEntry, runErr error) error {
	if entry == nil || entry.FinalizationLease == nil || runErr == nil {
		return nil
	}
	proof := entry.FinalizationLease
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
		code, proof.rowID, proof.Token, code == "continuation_driver_unavailable", proof.leasedBy, proof.leasedAt, proof.attempts, time.Now().Unix())
	return err
}
