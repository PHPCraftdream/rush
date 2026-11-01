// Durable queued-work purge for stopped sessions (task #1153): removes every
// row that could make the next process's RunQueuePump resurrect a session
// the operator stopped via kill / cancel / reset --force.

package session

import (
	"context"
)

// PurgedQueuedWork reports how many durable queued-work rows of one session
// were removed by PurgeQueuedWorkForSession, per table.
type PurgedQueuedWork struct {
	RunQueue      int64
	PendingInject int64
	OrphanOutbox  int64
}

// PurgeQueuedWorkForSession deletes every durable queued-work row of
// sessionID in one transaction: all session_run_queue entries (both pending
// and leased — a caller that proved the session's holder dead also proves the
// lease owner is dead), all pending_injects rows (merge and interrupt), and
// all orphan_call_outbox fallback rows (the pump would otherwise move those
// back into the run queue and resurrect the session). Used by
// `rush sessions kill`, `rush sessions cancel`, and `rush sessions reset
// --force` so a stopped session cannot be restarted by the RunQueuePump of
// the next process on the same database. The messages rows behind inject
// signals are NOT touched: kill must not destroy conversation history, only
// the "something should run this" signals.
//
// The session execution requirement and credential marker rows are
// deliberately preserved: both are acked metadata and invisible to queue
// selections; deleting the session cascades them.
func (s *service) PurgeQueuedWorkForSession(ctx context.Context, sessionID string) (PurgedQueuedWork, error) {
	var purged PurgedQueuedWork
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return purged, err
	}
	defer tx.Rollback() //nolint:errcheck

	for _, step := range []struct {
		query  string
		args   []any
		target *int64
	}{
		{
			`DELETE FROM session_run_queue WHERE session_id = ? AND id NOT IN (?, ?)`,
			[]any{sessionID, SessionExecutionRequirementRowID(sessionID), SessionCredentialMarkerRowID(sessionID)},
			&purged.RunQueue,
		},
		{`DELETE FROM pending_injects WHERE session_id = ?`, []any{sessionID}, &purged.PendingInject},
		{`DELETE FROM orphan_call_outbox WHERE session_id = ?`, []any{sessionID}, &purged.OrphanOutbox},
	} {
		res, execErr := tx.ExecContext(ctx, step.query, step.args...)
		if execErr != nil {
			return purged, execErr
		}
		n, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return purged, rowsErr
		}
		*step.target = n
	}
	if err := tx.Commit(); err != nil {
		return purged, err
	}
	return purged, nil
}
