package session

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/google/uuid"
)

// BatchStopOwners fences each currently-live root for the requested owners.
func (s *BatchStore) BatchStopOwners(ctx context.Context, owners []string) ([]BatchControlTarget, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return nil, err
	}
	targets := make([]BatchControlTarget, 0)
	for _, owner := range owners {
		if owner == "" {
			continue
		}
		roots, err := q.ListLiveBatchRootsForOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, root := range roots {
			var exists int
			if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM batch_control_requests WHERE owner_session_id = ? AND host_id = ? AND root_id = ? AND node_id = ?)`, owner, root.HostID, root.RootID, root.RootID).Scan(&exists); err != nil {
				return nil, err
			}
			if exists == 0 {
				if _, err = q.CreateBatchControlRequest(ctx, db.CreateBatchControlRequestParams{RequestID: uuid.NewString(), OwnerSessionID: owner, HostID: root.HostID, RootID: root.RootID, NodeID: root.RootID, CreatedAt: time.Now().Unix()}); err != nil {
					return nil, err
				}
			}
			if _, err = tx.ExecContext(ctx, `UPDATE batch_nodes SET wake = 0, updated_at = ? WHERE owner_session_id = ? AND root_id = ? AND notice_selected = 1 AND wake = 1`, time.Now().Unix(), owner, root.RootID); err != nil {
				return nil, err
			}
			targets = append(targets, BatchControlTarget{Owner: owner, Host: root.HostID, Root: root.RootID, Node: root.RootID})
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return targets, nil
}
