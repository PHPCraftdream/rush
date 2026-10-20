package session

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Typed bind failures. Every one fails closed: the child session must not
// start, and the task-node row is left untouched.
var (
	// ErrBatchBindNodeUnknown: no task node under this (owner, root, node)
	// identity. Also covers a foreign owner.
	ErrBatchBindNodeUnknown = errors.New("batch bind: task node not found")
	// ErrBatchBindForeignIdentity: the node row names another host or
	// another Runner incarnation than the caller.
	ErrBatchBindForeignIdentity = errors.New("batch bind: node held by another host or runner incarnation")
	// ErrBatchBindStaleToken: the node's incarnation token differs.
	ErrBatchBindStaleToken = errors.New("batch bind: stale node token")
	// ErrBatchBindNodeKind: the node is a batch node, not a delegation task.
	ErrBatchBindNodeKind = errors.New("batch bind: node is not a task")
	// ErrBatchBindNodeNotLive: the task node already left starting/running.
	ErrBatchBindNodeNotLive = errors.New("batch bind: task node is not live")
	// ErrBatchBindRootNotLive: the root is terminal, cancelling or voided.
	ErrBatchBindRootNotLive = errors.New("batch bind: root is terminal or voided")
	// ErrBatchBindChildConflict: the node is already durably bound to a
	// DIFFERENT child session. Rebinding the SAME child is idempotent.
	ErrBatchBindChildConflict = errors.New("batch bind: node already bound to a different child session")
	// ErrBatchBindChildForeign: the child session does not exist or its
	// parent_session_id is not the owning session.
	ErrBatchBindChildForeign = errors.New("batch bind: child session missing or not owned by the delegating session")
	// ErrBatchBindLostUpdate: the fenced UPDATE matched no row although the
	// pre-read classification passed; the identity raced between read and
	// write, so the bind fails closed.
	ErrBatchBindLostUpdate = errors.New("batch bind: fenced update matched no row")
)

// BatchBindChildSession persists the delegation child-session link on a task
// node BEFORE the child session runs (task #1490). It writes only the
// immutable linkage columns child_session_id and updated_at — never state,
// cause, outcome, admission, notices or any sequence column — so the host
// event loop stays the single terminal authority and the projection SQL's
// COALESCE (ProjectBatchNode) preserves the link across later NULL
// projections instead of clearing it.
//
// The write is fenced on the full reservation identity: owner session, host,
// Runner incarnation, root, node, kind='task', incarnation token, a live
// node state ('starting'/'running'), a live root ('pending'/'starting'/
// 'running', not history-voided), and a child session row whose
// parent_session_id names the owning session. The history-void
// representation of a root row is notice_selected=1 AND delivery='void'
// (batch_history_rerun.go) and is refused here.
//
// The bind legitimately precedes the started announcement, so no announced
// requirement is imposed. Rebinding the SAME child is idempotent; a
// different child fails with ErrBatchBindChildConflict. Any zero-row fenced
// update after a passing classification is ErrBatchBindLostUpdate and fails
// closed.
func (s *BatchStore) BatchBindChildSession(ctx context.Context, owner, host, runner, root, node string, token uint64, child string) error {
	if token >= batchCommitInt63 {
		return ErrBatchValueOverflow
	}
	tok := int64(token)
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var kind, rowHost, rowRunner, rowState string
	var rowToken int64
	var rowChild sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT kind, host_id, runner_id, token, state, child_session_id FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND node_id = ?`,
		owner, root, node,
	).Scan(&kind, &rowHost, &rowRunner, &rowToken, &rowState, &rowChild)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBatchBindNodeUnknown
	}
	if err != nil {
		return err
	}
	switch {
	case kind != "task":
		return ErrBatchBindNodeKind
	case rowHost != host || rowRunner != runner:
		return ErrBatchBindForeignIdentity
	case rowToken != tok:
		return ErrBatchBindStaleToken
	case rowState != "starting" && rowState != "running":
		return ErrBatchBindNodeNotLive
	case rowChild.Valid && rowChild.String != child:
		return ErrBatchBindChildConflict
	}

	var rootState, delivery string
	err = tx.QueryRowContext(ctx,
		`SELECT state, delivery FROM batch_nodes WHERE root_id = ? AND node_id = root_id AND owner_session_id = ?`,
		root, owner,
	).Scan(&rootState, &delivery)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBatchBindRootNotLive
	}
	if err != nil {
		return err
	}
	liveRoot := rootState == "pending" || rootState == "starting" || rootState == "running"
	if !liveRoot || delivery == "void" {
		return ErrBatchBindRootNotLive
	}

	var parent sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT parent_session_id FROM sessions WHERE id = ?`,
		child,
	).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBatchBindChildForeign
	}
	if err != nil {
		return err
	}
	if !parent.Valid || parent.String != owner {
		return ErrBatchBindChildForeign
	}

	res, err := tx.ExecContext(ctx, `
UPDATE batch_nodes
SET child_session_id = ?, updated_at = ?
WHERE owner_session_id = ? AND host_id = ? AND runner_id = ?
  AND root_id = ? AND node_id = ?
  AND kind = 'task' AND token = ?
  AND state IN ('starting', 'running')
  AND (child_session_id IS NULL OR child_session_id = ?)
  AND EXISTS (
      SELECT 1 FROM batch_nodes r
      WHERE r.node_id = batch_nodes.root_id
        AND r.root_id = batch_nodes.root_id
        AND r.owner_session_id = batch_nodes.owner_session_id
        AND r.host_id = batch_nodes.host_id
        AND r.runner_id = batch_nodes.runner_id
        AND r.state IN ('pending', 'starting', 'running')
        AND NOT (r.notice_selected = 1 AND r.delivery = 'void')
  )
  AND EXISTS (
      SELECT 1 FROM sessions s
      WHERE s.id = ?
        AND s.parent_session_id = batch_nodes.owner_session_id
  )`,
		child, time.Now().Unix(), owner, host, runner, root, node, tok, child, child)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrBatchBindLostUpdate
	}
	return tx.Commit()
}
