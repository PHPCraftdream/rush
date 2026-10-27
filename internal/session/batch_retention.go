package session

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// BatchSettleCLIWakeDebt settles wake debt after the CLI has delivered the
// terminal result to its caller. CLI notices are not consumed by the interactive
// reaction channel, so the owning CLI result path must close this debt.
func (s *BatchStore) BatchSettleCLIWakeDebt(ctx context.Context, owner, root string) error {
	_, err := s.writer.ExecContext(ctx, `UPDATE batch_nodes SET delivery = 'done', wake = 0, reacted = 1, updated_at = ?
WHERE owner_session_id = ? AND root_id = ? AND notice_selected = 1
  AND delivery = 'pending' AND EXISTS (
    SELECT 1 FROM batch_nodes r WHERE r.node_id = batch_nodes.root_id AND r.origin_cli = 1
  )`, time.Now().Unix(), owner, root)
	return err
}

// Use the same explicit cutoff for dry-run and purge to avoid clock drift.
func (s *BatchStore) BatchCountExpired(ctx context.Context, cutoff int64) (int64, error) {
	roots, err := db.New(s.writer).BatchRetentionRoots(ctx, cutoff)
	return int64(len(roots)), err
}

// BatchPurgeExpired removes only whole terminal, debt-free, idle delegations.
// FK cascades remove node/history identity links and controls, not history.
// Legacy notice purge and host/driver cleanup are deliberately not invoked.
func (s *BatchStore) BatchPurgeExpired(ctx context.Context, cutoff int64) (int64, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return 0, err
	}
	roots, err := q.BatchRetentionRoots(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	var count int64
	for _, r := range roots {
		n, e := q.DeleteBatchSubtree(ctx, db.DeleteBatchSubtreeParams{RootID: r.RootID, NodeID: r.NodeID, HostID: r.HostID, OwnerSessionID: r.OwnerSessionID})
		if e != nil {
			return 0, e
		}
		count += n
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// BatchRetentionCutoff shares the legacy seven-day policy without its purge path.
func BatchRetentionCutoff(now time.Time) int64 { return now.Add(-AsyncDataRetentionAge).Unix() }

// BatchCountJobsOlderThan is the inert caller-chosen age dry-run wrapper.
func (s *BatchStore) BatchCountJobsOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	return s.BatchCountExpired(ctx, time.Now().Add(-age).Unix())
}

// BatchPurgeJobsOlderThan is the inert caller-chosen age purge wrapper.
func (s *BatchStore) BatchPurgeJobsOlderThan(ctx context.Context, age time.Duration) (int64, error) {
	return s.BatchPurgeExpired(ctx, time.Now().Add(-age).Unix())
}
