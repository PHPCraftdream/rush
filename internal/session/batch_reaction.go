package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// BatchDebtNodeRef fences a delivery, not a provider-reusable tool call.
type (
	BatchDebtNodeRef struct{ RootID, NodeID, NoticeMessageID string }
	// BatchDebtSnapshot includes retained question/notice debt without job lookup.
	BatchDebtSnapshot struct {
		Owner   string
		Nodes   []BatchDebtNodeRef
		Notices []DebtNoticeRef
	}
)

func (d BatchDebtSnapshot) Empty() bool { return len(d.Nodes) == 0 && len(d.Notices) == 0 }

// BatchPendingDebt names pending-inclusive selected-node and retained debt.
type BatchPendingDebt struct {
	Nodes   []BatchDebtNodeRef
	Notices []PendingNoticeDebt
}

// batchOutboxDebtNodesVoidAware mirrors the canonical BatchOutboxDebtNodes
// predicate (notice_selected=1, root announced, wake=1, reacted=0, delivery
// IN ('pending','done'), same ORDER BY) plus the R12-F root-void exclusion
// (r.delivery != 'void'): a root durably retired by a rerun is never debt.
// Expressed Go-side because this fork has no pinned sqlc regeneration path;
// fold back into internal/db/sql/batch_outbox.sql when regeneration is
// available. Delivery is carried alongside the identity for the caller's
// visible filter.
//
// The extra verbatim n.delivery != 'void' term repeats idx_batch_nodes_debt's
// partial-index predicate so the planner serves the read from that index
// instead of scanning batch_nodes (the IN list does not imply the inequality
// to SQLite); it cannot change the result set, which the IN term already
// fixes to ('pending','done').
type batchDebtNodeRow struct {
	ref      BatchDebtNodeRef
	delivery string
}

// batchDebtNodesVoidAwareSQL is the exact statement
// batchOutboxDebtNodesVoidAware executes; package-level so the plan test
// pins the production SQL, never a copy.
// Expressed as a package constant (Go-side) because this fork has no pinned sqlc regeneration path;
// fold back into internal/db/sql/batch_outbox.sql when regeneration is available.
const batchDebtNodesVoidAwareSQL = `SELECT n.root_id, n.node_id, COALESCE(n.notice_message_id, '') AS notice_message_id, n.delivery FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.owner_session_id = ? AND n.notice_selected = 1 AND r.announced = 1
  AND r.delivery != 'void'
  AND n.wake = 1 AND n.reacted = 0 AND n.delivery IN ('pending', 'done')
  AND n.delivery != 'void'
ORDER BY n.root_id, n.notice_sequence, n.notice_ordinal, n.node_id`

func batchOutboxDebtNodesVoidAware(ctx context.Context, dbtx db.DBTX, owner string) ([]batchDebtNodeRow, error) {
	rows, err := dbtx.QueryContext(ctx, batchDebtNodesVoidAwareSQL, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []batchDebtNodeRow{}
	for rows.Next() {
		var row batchDebtNodeRow
		if err := rows.Scan(&row.ref.RootID, &row.ref.NodeID, &row.ref.NoticeMessageID, &row.delivery); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// batchOutboxReactNodesVoidAware is the generated BatchOutboxReactNodes
// (internal/db/sql/batch_outbox.sql) with one addition: the verbatim
// `delivery != 'void'` term. This fork has no pinned sqlc regeneration path
// (same precedent as batchOutboxPendingVoidAware), so the statement lives
// here; the added term is idx_batch_nodes_debt's partial-index predicate
// spelled out, which lets the planner mark the owner's reacted rows from the
// index instead of scanning batch_nodes as the table grows. The result set
// is unchanged: delivery = 'done' already excludes 'void'.
const batchOutboxReactNodesVoidAware = `UPDATE batch_nodes SET reacted = 1, updated_at = ?
WHERE batch_nodes.owner_session_id = ? AND notice_selected = 1 AND batch_nodes.delivery = 'done'
  AND batch_nodes.delivery != 'void' AND batch_nodes.wake = 1 AND batch_nodes.reacted = 0
  AND EXISTS (SELECT 1 FROM batch_nodes r WHERE r.node_id = batch_nodes.root_id AND r.announced = 1)`

func batchDebtSnapshot(ctx context.Context, dbtx db.DBTX, owner string, visible bool) (BatchDebtSnapshot, error) {
	nodes, err := batchOutboxDebtNodesVoidAware(ctx, dbtx, owner)
	if err != nil {
		return BatchDebtSnapshot{}, err
	}
	q := db.New(dbtx)
	notices, err := q.BatchOutboxDebtNotices(ctx, owner)
	if err != nil {
		return BatchDebtSnapshot{}, err
	}
	snap := BatchDebtSnapshot{Owner: owner}
	for _, n := range nodes {
		if !visible || n.delivery == "done" {
			snap.Nodes = append(snap.Nodes, n.ref)
		}
	}
	for _, n := range notices {
		if !visible || n.Delivery == "done" {
			snap.Notices = append(snap.Notices, DebtNoticeRef{ID: n.ID, NoticeMessageID: n.NoticeMessageID.String})
		}
	}
	return snap, nil
}

// BatchReactionCaptureDebtSnapshot pins both tables in one read transaction.
func (s *BatchStore) BatchReactionCaptureDebtSnapshot(ctx context.Context, owner string) (BatchDebtSnapshot, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BatchDebtSnapshot{}, err
	}
	defer tx.Rollback()
	snap, err := batchDebtSnapshot(ctx, tx, owner, true)
	if err != nil {
		return BatchDebtSnapshot{}, err
	}
	return snap, tx.Commit()
}

func (s *BatchStore) batchReactionDebtExists(ctx context.Context, owner string, visible bool) (bool, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	snap, err := batchDebtSnapshot(ctx, tx, owner, visible)
	if err != nil {
		return false, err
	}
	return !snap.Empty(), tx.Commit()
}

func (s *BatchStore) BatchReactionDebtExists(ctx context.Context, owner string) (bool, error) {
	return s.batchReactionDebtExists(ctx, owner, false)
}

func (s *BatchStore) BatchReactionVisibleDebtExists(ctx context.Context, owner string) (bool, error) {
	return s.batchReactionDebtExists(ctx, owner, true)
}

func (s *BatchStore) BatchReactionPendingInclusiveDebtRows(ctx context.Context, owner string) (BatchPendingDebt, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return BatchPendingDebt{}, err
	}
	defer tx.Rollback()
	out, err := batchReactionPendingInclusiveDebtRows(ctx, tx, owner)
	if err != nil {
		return BatchPendingDebt{}, err
	}
	return out, tx.Commit()
}

// batchReactionPendingInclusiveDebtRows is the transaction body of
// BatchReactionPendingInclusiveDebtRows: the node debt rows and the notices
// rows are read EXACTLY once each inside the caller's transaction (the
// previous shape read the notices twice — once through batchDebtSnapshot,
// once for the kinds).
func batchReactionPendingInclusiveDebtRows(ctx context.Context, dbtx db.DBTX, owner string) (BatchPendingDebt, error) {
	nodes, err := batchOutboxDebtNodesVoidAware(ctx, dbtx, owner)
	if err != nil {
		return BatchPendingDebt{}, err
	}
	notices, err := db.New(dbtx).BatchOutboxDebtNotices(ctx, owner)
	if err != nil {
		return BatchPendingDebt{}, err
	}
	out := BatchPendingDebt{}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, n.ref)
	}
	for _, n := range notices {
		out.Notices = append(out.Notices, PendingNoticeDebt{ID: n.ID, Kind: n.Kind})
	}
	return out, nil
}

func (s *BatchStore) BatchReactionPendingInclusiveDebtSummary(ctx context.Context, owner string) (bool, []string, error) {
	rows, err := s.BatchReactionPendingInclusiveDebtRows(ctx, owner)
	var kinds []string
	for _, n := range rows.Notices {
		kinds = append(kinds, n.Kind)
	}
	return len(rows.Nodes) > 0, kinds, err
}

func (s *BatchStore) BatchReactionDebtOwners(ctx context.Context, owners []string) (map[string]bool, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make(map[string]bool)
	for _, owner := range owners {
		snap, err := batchDebtSnapshot(ctx, tx, owner, false)
		if err != nil {
			return nil, err
		}
		if !snap.Empty() {
			out[owner] = true
		}
	}
	return out, tx.Commit()
}

// BatchReactionMarkReactedWithMessageUpdate must be called only for real
// content (including an awaiting-answer question), never error/cancel/empty.
// It updates history and both debt tables atomically, publishing after commit.
func (s *BatchStore) BatchReactionMarkReactedWithMessageUpdate(ctx context.Context, messages message.Service, owner string, msg message.Message) error {
	if msg.SessionID != owner {
		return ErrBatchProjectionConflict
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Take the writer before UpdateTx so accounting cannot race another writer.
	q := db.New(tx)
	now := time.Now().Unix()
	if _, err = tx.ExecContext(ctx, batchOutboxReactNodesVoidAware, now, owner); err != nil {
		return err
	}
	persisted, err := q.GetMessage(ctx, msg.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return errBatchReactionMessageGone
	}
	if err != nil {
		return err
	}
	if persisted.SessionID != owner {
		return ErrBatchProjectionConflict
	}
	publish, err := messages.UpdateTx(ctx, tx, msg)
	if err != nil {
		return err
	}
	if _, err = q.MarkSessionNoticesReactedForOwner(ctx, db.MarkSessionNoticesReactedForOwnerParams{Owner: owner, UpdatedAt: now}); err != nil {
		return err
	}
	if _, err = q.BatchOutboxClearFailure(ctx, db.BatchOutboxClearFailureParams{Owner: owner, UpdatedAt: now}); err != nil {
		return err
	}
	if _, err = q.ClearSessionNoticesReactedFailedForOwner(ctx, db.ClearSessionNoticesReactedFailedForOwnerParams{Owner: owner, UpdatedAt: now}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if publish != nil {
		publish()
	}
	return nil
}

func batchReactionAccountSnapshot(ctx context.Context, q *db.Queries, owner string, snap BatchDebtSnapshot, increment, failed int64) (int64, error) {
	if snap.Owner != owner {
		return 0, ErrBatchProjectionConflict
	}
	var total int64
	now := time.Now().Unix()
	for _, n := range snap.Nodes {
		rows, err := q.BatchOutboxAccountNode(ctx, db.BatchOutboxAccountNodeParams{Owner: owner, RootID: n.RootID, NodeID: n.NodeID, MessageID: sql.NullString{String: n.NoticeMessageID, Valid: true}, Increment: increment, Reacted: failed, Failed: failed, UpdatedAt: now})
		if err != nil {
			return 0, err
		}
		total += rows
	}
	for _, n := range snap.Notices {
		rows, err := q.BatchOutboxAccountNotice(ctx, db.BatchOutboxAccountNoticeParams{Owner: owner, ID: n.ID, MessageID: sql.NullString{String: n.NoticeMessageID, Valid: true}, Increment: increment, Reacted: failed, Failed: failed, UpdatedAt: now})
		if err != nil {
			return 0, err
		}
		total += rows
	}
	return total, nil
}

func (s *BatchStore) batchReactionAccount(ctx context.Context, owner string, snap BatchDebtSnapshot, increment, failed int64, marker *string) (int64, error) {
	if snap.Empty() {
		return 0, nil
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	count, err := batchReactionAccountSnapshot(ctx, q, owner, snap, increment, failed)
	if err != nil {
		return 0, err
	}
	if count > 0 && marker != nil {
		now := time.Now().Unix()
		_, err = q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: owner, Kind: NoticeKindWakeFailed, Text: *marker, CreatedAt: now, UpdatedAt: now})
		if err != nil {
			return 0, err
		}
	}
	return count, tx.Commit()
}

func (s *BatchStore) BatchReactionIncrementWakeAttempts(ctx context.Context, owner string, snap BatchDebtSnapshot) error {
	_, err := s.batchReactionAccount(ctx, owner, snap, 1, 0, nil)
	return err
}

func (s *BatchStore) BatchReactionSettleReactedFailed(ctx context.Context, owner string, snap BatchDebtSnapshot) error {
	_, err := s.batchReactionAccount(ctx, owner, snap, 0, 1, nil)
	return err
}

func (s *BatchStore) BatchReactionSettleReactedFailedWithMarker(ctx context.Context, owner string, snap BatchDebtSnapshot, text string) (int64, error) {
	return s.batchReactionAccount(ctx, owner, snap, 0, 1, &text)
}

// BatchReactionMaxWakeAttempts excludes replaced, re-pulled and void deliveries.
func (s *BatchStore) BatchReactionMaxWakeAttempts(ctx context.Context, owner string, snap BatchDebtSnapshot) (int, error) {
	if snap.Empty() {
		return 0, nil
	}
	if snap.Owner != owner {
		return 0, ErrBatchProjectionConflict
	}
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	max := 0
	current, err := batchDebtSnapshot(ctx, tx, owner, true)
	if err != nil {
		return 0, err
	}
	liveNodes := make(map[BatchDebtNodeRef]bool)
	for _, n := range current.Nodes {
		liveNodes[n] = true
	}
	liveNotices := make(map[DebtNoticeRef]bool)
	for _, n := range current.Notices {
		liveNotices[n] = true
	}
	for _, n := range snap.Nodes {
		if !liveNodes[n] {
			continue
		}
		row, err := q.GetBatchNode(ctx, db.GetBatchNodeParams{OwnerSessionID: owner, RootID: n.RootID, NodeID: n.NodeID})
		if err != nil {
			return 0, err
		}
		if int(row.WakeAttempts) > max {
			max = int(row.WakeAttempts)
		}
	}
	for _, n := range snap.Notices {
		if !liveNotices[n] {
			continue
		}
		row, err := q.GetSessionNotice(ctx, n.ID)
		if err != nil {
			return 0, err
		}
		if int(row.WakeAttempts) > max {
			max = int(row.WakeAttempts)
		}
	}
	return max, tx.Commit()
}

// BatchReactedFailedText retains node identity for later parent consumers.
type BatchReactedFailedText struct {
	RootID, NodeID, Text string
	NoticeID             int64
}

func (s *BatchStore) BatchReactionListReactedFailedText(ctx context.Context, owner string) ([]BatchReactedFailedText, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	nodes, err := q.BatchOutboxFailedNodes(ctx, owner)
	if err != nil {
		return nil, err
	}
	notices, err := q.ListReactedFailedSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	var out []BatchReactedFailedText
	for _, n := range nodes {
		out = append(out, BatchReactedFailedText{RootID: n.RootID, NodeID: n.NodeID, Text: n.OutcomeSummary})
	}
	for _, n := range notices {
		out = append(out, BatchReactedFailedText{NoticeID: n.ID, Text: n.Text})
	}
	return out, tx.Commit()
}

func (s *BatchStore) BatchReactionSetWakeZeroForOwners(ctx context.Context, owners []string) error {
	if len(owners) == 0 {
		return nil
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := db.New(tx)
	now := time.Now().Unix()
	for _, owner := range owners {
		if _, err = q.BatchOutboxWakeZero(ctx, db.BatchOutboxWakeZeroParams{Owner: owner, UpdatedAt: now}); err != nil {
			return err
		}
	}
	_, err = q.SetSessionNoticesWakeZeroPendingForOwners(ctx, db.SetSessionNoticesWakeZeroPendingForOwnersParams{OwnerIds: owners, UpdatedAt: now})
	if err != nil {
		return err
	}
	return tx.Commit()
}

// A missing history message cannot justify closing visible reaction debt.
var errBatchReactionMessageGone = errors.New("batch reaction message disappeared")
