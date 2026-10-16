package session

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

type BatchCLIIdentity struct{ Owner, Root, Host, Runner, State string }

func (s *BatchStore) BatchCLIIdentities(ctx context.Context) ([]BatchCLIIdentity, error) {
	rows, err := s.writer.QueryContext(ctx, `SELECT owner_session_id,root_id,host_id,runner_id,state FROM batch_nodes WHERE node_id=root_id ORDER BY created_at,root_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BatchCLIIdentity{}
	for rows.Next() {
		var x BatchCLIIdentity
		if err = rows.Scan(&x.Owner, &x.Root, &x.Host, &x.Runner, &x.State); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *BatchStore) BatchCLIFind(ctx context.Context, prefix string) (BatchCLIIdentity, string, error) {
	if strings.TrimSpace(prefix) == "" {
		return BatchCLIIdentity{}, "", errors.New("batch target is empty")
	}
	// Exact nodes (including roots) take precedence over prefix matches.
	var x BatchCLIIdentity
	var node string
	err := s.writer.QueryRowContext(ctx, `SELECT owner_session_id,root_id,host_id,runner_id,node_id,state FROM batch_nodes WHERE node_id=?`, prefix).Scan(&x.Owner, &x.Root, &x.Host, &x.Runner, &node, &x.State)
	if err == nil {
		return x, node, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return BatchCLIIdentity{}, "", err
	}
	rows, err := s.writer.QueryContext(ctx, `SELECT owner_session_id,root_id,host_id,runner_id,node_id,state FROM batch_nodes WHERE node_id=root_id AND node_id LIKE ? ESCAPE '\' ORDER BY root_id`, strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(prefix)+"%")
	if err != nil {
		return BatchCLIIdentity{}, "", err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if err = rows.Scan(&x.Owner, &x.Root, &x.Host, &x.Runner, &node, &x.State); err != nil {
			return BatchCLIIdentity{}, "", err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return BatchCLIIdentity{}, "", err
	}
	if count == 0 {
		return BatchCLIIdentity{}, "", sql.ErrNoRows
	}
	if count > 1 {
		return BatchCLIIdentity{}, "", errors.New("batch target prefix is ambiguous")
	}
	return x, node, nil
}

func (s *BatchStore) BatchCLIGetNode(ctx context.Context, owner, root, node string) (db.BatchNode, error) {
	return db.New(s.writer).GetBatchNode(ctx, db.GetBatchNodeParams{OwnerSessionID: owner, RootID: root, NodeID: node})
}

func (s *BatchStore) BatchCLIPollControl(ctx context.Context, t BatchControlTarget, id string, wait time.Duration) (db.BatchControlRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return s.BatchWaitControl(ctx, t, id)
}
