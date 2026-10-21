package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

type BatchStopOwnerTarget struct {
	BatchControlTarget
	RequestID string
	RunnerID  string
	Status    string
}

// BatchStopOwners fences each currently-live root for the requested owners.
func (s *BatchStore) BatchStopOwners(ctx context.Context, owners []string) ([]BatchStopOwnerTarget, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return nil, err
	}
	targets := make([]BatchStopOwnerTarget, 0)
	for _, owner := range owners {
		if owner == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE batch_control_requests
			SET status = 'answered', answer_code = 'accepted',
				answer_sequence = (SELECT root_sequence FROM batch_nodes r WHERE r.root_id = batch_control_requests.root_id AND r.node_id = r.root_id AND r.owner_session_id = batch_control_requests.owner_session_id AND r.host_id = batch_control_requests.host_id AND r.runner_id = batch_control_requests.runner_id),
				answered_at = ?
			WHERE `+psfOwnerStopSQLPredicate("batch_control_requests")+` AND owner_session_id = ? AND node_id = root_id AND operation = 'stop' AND status = 'pending'
				AND EXISTS (SELECT 1 FROM batch_nodes r WHERE r.root_id = batch_control_requests.root_id AND r.node_id = r.root_id AND r.owner_session_id = batch_control_requests.owner_session_id AND r.host_id = batch_control_requests.host_id AND r.runner_id = batch_control_requests.runner_id AND r.state IN ('completed', 'failed', 'cancelled', 'timed_out', 'interrupted', 'skipped') AND r.root_sequence > 0)`, time.Now().Unix(), owner); err != nil {
			return nil, err
		}
		roots, err := q.ListLiveBatchRootsForOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, root := range roots {
			var requestID, runnerID, status string
			err := tx.QueryRowContext(ctx, `SELECT request_id, runner_id, status FROM batch_control_requests WHERE owner_session_id = ? AND host_id = ? AND root_id = ? AND node_id = root_id AND runner_id = ? AND `+psfOwnerStopSQLPredicate("")+` ORDER BY created_at, request_id LIMIT 1`, owner, root.HostID, root.RootID, root.RunnerID).Scan(&requestID, &runnerID, &status)
			if errors.Is(err, sql.ErrNoRows) {
				requestID = psfOwnerStopRequestID()
				result, createErr := tx.ExecContext(ctx, `INSERT INTO batch_control_requests (request_id, owner_session_id, root_id, node_id, host_id, operation, status, answer_code, answer_sequence, created_at, answered_at, runner_id)
					SELECT ?, ?, ?, ?, ?, 'stop', 'pending', '', NULL, ?, NULL, r.runner_id FROM batch_nodes r
					WHERE r.root_id = ? AND r.node_id = r.root_id AND r.owner_session_id = ? AND r.host_id = ? AND r.runner_id = ?`, requestID, owner, root.RootID, root.RootID, root.HostID, time.Now().Unix(), root.RootID, owner, root.HostID, root.RunnerID)
				if createErr != nil {
					return nil, createErr
				}
				n, affectedErr := result.RowsAffected()
				if affectedErr != nil {
					return nil, affectedErr
				}
				if n != 1 {
					return nil, ErrBatchProjectionConflict
				}
				runnerID, status = root.RunnerID, "pending"
			} else if err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE batch_nodes SET wake = 0, updated_at = ? WHERE owner_session_id = ? AND host_id = ? AND root_id = ? AND runner_id = ? AND notice_selected = 1 AND wake = 1`, time.Now().Unix(), owner, root.HostID, root.RootID, root.RunnerID); err != nil {
				return nil, err
			}
			targets = append(targets, BatchStopOwnerTarget{BatchControlTarget: BatchControlTarget{Owner: owner, Host: root.HostID, Root: root.RootID, Node: root.RootID}, RequestID: requestID, RunnerID: runnerID, Status: status})
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return targets, nil
}

// BatchAnswerOwnerStop acknowledges the exact durable stop request after a successful cancellation.
func (s *BatchStore) BatchAnswerOwnerStop(ctx context.Context, target BatchStopOwnerTarget) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return err
	}
	request, err := q.GetBatchControlRequest(ctx, db.GetBatchControlRequestParams{RequestID: target.RequestID, OwnerSessionID: target.Owner, HostID: target.Host, RootID: target.Root})
	if err != nil {
		return err
	}
	if !psfIsOwnerStopRequestID(request.RequestID) || request.NodeID != target.Node || request.NodeID != request.RootID || request.RunnerID != target.RunnerID {
		return ErrBatchControlStale
	}
	root, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{RootID: target.Root, OwnerSessionID: target.Owner})
	if err != nil {
		return err
	}
	if root.RunnerID != target.RunnerID || root.HostID != target.Host {
		return ErrBatchControlStale
	}
	if request.Status == "answered" {
		return tx.Commit()
	}
	if root.RootSequence <= 0 {
		return ErrBatchProjectionConflict
	}
	if err = batchRequireRow(q.AnswerBatchControlRequest(ctx, db.AnswerBatchControlRequestParams{RequestID: target.RequestID, OwnerSessionID: target.Owner, HostID: target.Host, RootID: target.Root, NodeID: target.Node, RunnerID: target.RunnerID, AnswerCode: "accepted", AnswerSequence: sql.NullInt64{Int64: root.RootSequence, Valid: true}, AnsweredAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}})); err != nil {
		return err
	}
	return tx.Commit()
}
