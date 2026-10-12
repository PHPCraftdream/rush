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

func batchDebtSnapshot(ctx context.Context, q *db.Queries, owner string, visible bool) (BatchDebtSnapshot, error) {
	nodes, err := q.BatchOutboxDebtNodes(ctx, owner)
	if err != nil {
		return BatchDebtSnapshot{}, err
	}
	notices, err := q.BatchOutboxDebtNotices(ctx, owner)
	if err != nil {
		return BatchDebtSnapshot{}, err
	}
	snap := BatchDebtSnapshot{Owner: owner}
	for _, n := range nodes {
		if !visible || n.Delivery == "done" {
			snap.Nodes = append(snap.Nodes, BatchDebtNodeRef{RootID: n.RootID, NodeID: n.NodeID, NoticeMessageID: n.NoticeMessageID.String})
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
	snap, err := batchDebtSnapshot(ctx, db.New(tx), owner, true)
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
	snap, err := batchDebtSnapshot(ctx, db.New(tx), owner, visible)
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
	q := db.New(tx)
	snap, err := batchDebtSnapshot(ctx, q, owner, false)
	if err != nil {
		return BatchPendingDebt{}, err
	}
	rows, err := q.BatchOutboxDebtNotices(ctx, owner)
	if err != nil {
		return BatchPendingDebt{}, err
	}
	out := BatchPendingDebt{Nodes: snap.Nodes}
	for _, n := range rows {
		out.Notices = append(out.Notices, PendingNoticeDebt{ID: n.ID, Kind: n.Kind})
	}
	return out, tx.Commit()
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
		snap, err := batchDebtSnapshot(ctx, db.New(tx), owner, false)
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
	if _, err = q.BatchOutboxReactNodes(ctx, db.BatchOutboxReactNodesParams{Owner: owner, UpdatedAt: now}); err != nil {
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
	current, err := batchDebtSnapshot(ctx, q, owner, true)
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
