package sessionbatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	sessionbatchread "github.com/PHPCraftdream/rush/internal/session/batch/sessionbatchread"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// BatchDebtNodeRef fences a delivery, not a provider-reusable tool call.
type (
	BatchDebtNodeRef struct{ RootID, NodeID, NoticeMessageID string }
	// BatchDebtSnapshot includes retained question/notice debt without job lookup.
	BatchDebtSnapshot struct {
		Owner   string
		Nodes   []BatchDebtNodeRef
		Notices []sessionnotice.DebtNoticeRef
	}
)

func (d BatchDebtSnapshot) Empty() bool { return len(d.Nodes) == 0 && len(d.Notices) == 0 }

// BatchPendingDebt names pending-inclusive selected-node and retained debt.
type BatchPendingDebt struct {
	Nodes   []BatchDebtNodeRef
	Notices []sessionnotice.PendingNoticeDebt
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

// ReadBatchDebtNodeRefs exposes the R8 outbox debt-node read (the exact
// batchOutboxDebtNodesVoidAware statement and scan) as identity-only
// refs for the root SessionNoticeStore's pending-inclusive debt read.
// Zero rows yield a nil slice, preserving the legacy append-from-nil
// shape of BatchPendingDebt.Nodes; errors are returned verbatim.
func ReadBatchDebtNodeRefs(ctx context.Context, dbtx db.DBTX, owner string) ([]BatchDebtNodeRef, error) {
	rows, err := batchOutboxDebtNodesVoidAware(ctx, dbtx, owner)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	refs := make([]BatchDebtNodeRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, row.ref)
	}
	return refs, nil
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

// batchClearFailureRootsSQL lists the owner's roots. Served by
// idx_batch_nodes_owner_roots (partial node_id = root_id, no state filter):
// per-owner bounded, never a scan of batch_nodes.
const batchClearFailureRootsSQL = `SELECT root_id FROM batch_nodes
WHERE owner_session_id = ?1 AND node_id = root_id`

// batchOwnerRootIDs collects the owner's live-root IDs through the
// index-served batchClearFailureRootsSQL (per-owner bounded by
// idx_batch_nodes_owner_roots, never a table scan).
func batchOwnerRootIDs(ctx context.Context, dbtx db.DBTX, owner string) ([]string, error) {
	rows, err := dbtx.QueryContext(ctx, batchClearFailureRootsSQL, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// batchOutboxClearFailureServed replaces the generated BatchOutboxClearFailure
// (internal/db/sql/batch_outbox.sql), whose owner-wide predicate matches no
// index and therefore scans batch_nodes as the table grows. The ROW SET is
// identical: the composite FK guarantees every node of a root shares the
// root's owner_session_id, so selecting by owner + root_id IN (owner's roots)
// hits exactly the same rows. Each match is served from
// idx_batch_nodes_parent's root_id prefix instead of a scan, and the root
// list itself is per-owner bounded by idx_batch_nodes_owner_roots. Chunks of
// sessionbatchread.BatchReadChunk roots keep the statement inside SQLite's variable limit.
func batchOutboxClearFailureServed(ctx context.Context, dbtx db.DBTX, owner string, now int64) error {
	rootIDs, err := batchOwnerRootIDs(ctx, dbtx, owner)
	if err != nil {
		return err
	}
	for start := 0; start < len(rootIDs); start += sessionbatchread.BatchReadChunk {
		chunk := rootIDs[start:min(start+sessionbatchread.BatchReadChunk, len(rootIDs))]
		stmt := `UPDATE batch_nodes SET reacted_failed = 0, updated_at = ?1 WHERE owner_session_id = ?2 AND reacted_failed = 1 AND root_id IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `)`
		args := make([]any, 0, len(chunk)+2)
		args = append(args, now, owner)
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := dbtx.ExecContext(ctx, stmt, args...); err != nil {
			return err
		}
	}
	return nil
}

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
			snap.Notices = append(snap.Notices, sessionnotice.DebtNoticeRef{ID: n.ID, NoticeMessageID: n.NoticeMessageID.String})
		}
	}
	return snap, nil
}

// BatchReactionCaptureDebtSnapshot pins both tables in one read transaction.
func (s *BatchStore) BatchReactionCaptureDebtSnapshot(ctx context.Context, owner string) (BatchDebtSnapshot, error) {
	tx, err := s.beginReadTx(ctx)
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
	tx, err := s.beginReadTx(ctx)
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
	tx, err := s.beginReadTx(ctx)
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
		out.Notices = append(out.Notices, sessionnotice.PendingNoticeDebt{ID: n.ID, Kind: n.Kind})
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
		return session.ErrBatchProjectionConflict
	}
	// See batch_delivery_store.go: prepare before the tx holds the pool's only
	// connection.
	if err := db.PdaBatchEnsure(ctx, s.writer); err != nil {
		return err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Take the writer before UpdateTx so accounting cannot race another writer.
	q := db.PdaBatchTxQueries(ctx, s.writer, tx)
	now := time.Now().Unix()
	if _, err = q.PdbBatchRawExec(ctx, batchOutboxReactNodesVoidAware, now, owner); err != nil {
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
		return session.ErrBatchProjectionConflict
	}
	publish, err := pdcUpdateTx(ctx, q, tx, messages, msg)
	if err != nil {
		return err
	}
	if _, err = q.MarkSessionNoticesReactedForOwner(ctx, db.MarkSessionNoticesReactedForOwnerParams{Owner: owner, UpdatedAt: now}); err != nil {
		return err
	}
	if err := batchOutboxClearFailureServed(ctx, tx, owner, now); err != nil {
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
		return 0, session.ErrBatchProjectionConflict
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
		_, err = q.InsertSessionNotice(ctx, db.InsertSessionNoticeParams{Owner: owner, Kind: sessionnotice.NoticeKindWakeFailed, Text: *marker, CreatedAt: now, UpdatedAt: now})
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
		return 0, session.ErrBatchProjectionConflict
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
	liveNotices := make(map[sessionnotice.DebtNoticeRef]bool)
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

// nullInt64Less reproduces SQLite's ASC NULLS-FIRST ordering for a nullable
// column: both null -> equal, a null and b not -> a first, else numeric.
func nullInt64Less(a, b sql.NullInt64) bool {
	switch {
	case !a.Valid && !b.Valid:
		return false
	case !a.Valid:
		return true
	case !b.Valid:
		return false
	default:
		return a.Int64 < b.Int64
	}
}

// batchFailedNodeRow is one failed notice-bearing node read from batch_nodes
// via the served two-step read below.
type batchFailedNodeRow struct {
	RootID, NodeID, Outcome string
	NoticeSeq, NoticeOrd    sql.NullInt64
}

// batchOutboxFailedNodesServed replaces the generated BatchOutboxFailedNodes
// (internal/db/sql/batch_outbox.sql), whose owner-wide predicate matches no
// index and therefore scans batch_nodes as the table grows. The ROW SET is
// identical: the composite FK guarantees every node of a root shares the
// root's owner_session_id, so selecting by owner + root_id IN (owner's roots)
// returns exactly the same rows. Each read is served from
// idx_batch_nodes_parent's root_id prefix, and the root list is per-owner
// bounded by idx_batch_nodes_owner_roots. Rows are sorted Go-side to
// reproduce the original ORDER BY (root_id, notice_sequence, notice_ordinal,
// node_id) exactly, including SQLite's NULLS-FIRST semantics for the two
// nullable columns.
func batchOutboxFailedNodesServed(ctx context.Context, dbtx db.DBTX, owner string) ([]BatchReactedFailedText, error) {
	rootIDs, err := batchOwnerRootIDs(ctx, dbtx, owner)
	if err != nil {
		return nil, err
	}
	var rows []batchFailedNodeRow
	for start := 0; start < len(rootIDs); start += sessionbatchread.BatchReadChunk {
		chunk := rootIDs[start:min(start+sessionbatchread.BatchReadChunk, len(rootIDs))]
		stmt := `SELECT root_id, node_id, notice_sequence, notice_ordinal, outcome_summary FROM batch_nodes WHERE owner_session_id = ?1 AND notice_selected = 1 AND reacted_failed = 1 AND root_id IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `)`
		args := make([]any, 0, len(chunk)+1)
		args = append(args, owner)
		for _, id := range chunk {
			args = append(args, id)
		}
		got, err := scanBatchFailedNodeRows(ctx, dbtx, stmt, args)
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].RootID != rows[j].RootID {
			return rows[i].RootID < rows[j].RootID
		}
		if rows[i].NoticeSeq != rows[j].NoticeSeq {
			return nullInt64Less(rows[i].NoticeSeq, rows[j].NoticeSeq)
		}
		if rows[i].NoticeOrd != rows[j].NoticeOrd {
			return nullInt64Less(rows[i].NoticeOrd, rows[j].NoticeOrd)
		}
		return rows[i].NodeID < rows[j].NodeID
	})
	out := make([]BatchReactedFailedText, 0, len(rows))
	for _, row := range rows {
		out = append(out, BatchReactedFailedText{RootID: row.RootID, NodeID: row.NodeID, Text: row.Outcome})
	}
	return out, nil
}

func (s *BatchStore) BatchReactionListReactedFailedText(ctx context.Context, owner string) ([]BatchReactedFailedText, error) {
	tx, err := s.writer.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	nodes, err := batchOutboxFailedNodesServed(ctx, tx, owner)
	if err != nil {
		return nil, err
	}
	notices, err := q.ListReactedFailedSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	var out []BatchReactedFailedText
	out = append(out, nodes...)
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

// scanBatchFailedNodeRows runs one failed-node chunk SELECT and scans it; the
// rows are closed by defer on every path.
func scanBatchFailedNodeRows(ctx context.Context, dbtx db.DBTX, stmt string, args []any) ([]batchFailedNodeRow, error) {
	qrows, err := dbtx.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer qrows.Close()
	var out []batchFailedNodeRow
	for qrows.Next() {
		var row batchFailedNodeRow
		if err := qrows.Scan(&row.RootID, &row.NodeID, &row.NoticeSeq, &row.NoticeOrd, &row.Outcome); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, qrows.Err()
}

// Package session's R9 additions are inert. There are no production calls;
// the batch stores are the live execution authority since the H4 cutover (the
// legacy async_jobs store was deleted).
// Recovery never executes, resumes, cancels, writes history, or wakes work.
// A dead UUID cannot revive. Root writer election precedes all snapshot reads;
// node outcomes, one root notice and pending-control closure commit together.
// Existing terminal outcomes and selected outboxes are immutable to recovery.
// Retention deletes whole roots and identity links, never unrelated history.
//
// Retention also protects pending controls: unresolved requester intent is not
// disposable history. Fully terminal dead roots close controls without changing
// nodes/outboxes. The live pump answers terminal/stale races without callbacks.
// Age wrappers share the cutoff predicate; BatchRecoverOwnerScope is inert.
//
// Mapping table (every legacy recovery/question/GC function and host counterpart):
//
// | Legacy function | R9 counterpart or retained/dropped reason |
// | RecoverDeadHost | BatchRecoverDeadHost; root-atomic, all four live states. |
// | removeDeadHostFile | Retained unchanged; R9 never invokes old job-only reap. |
// | recoveredResultText | Dropped: host_lost fact, no inferred execution result. |
// | recoveredDelegationText | Dropped: committed envelopes remain; no prose read. |
// | recoveredQuestionText | Dropped: held input is not recovered completion. |
// | SweepDeadHosts | BatchSweepDeadHosts with empty owner filter. |
// | RecoverOwnerScope | BatchSweepDeadHosts with owner host-discovery filter. |
// | PurgeExpired | BatchPurgeExpired; old notice/driver/file paths unchanged. |
// | purgeEmptyDeadHostFiles | Retained unchanged, not called: unsafe job-only gate. |
// | purgeOrphanHostLockFiles | Retained unchanged; no new R9 file cleanup. |
// | reapOrphanHostLock | Retained unchanged; no new R9 orphan cleanup. |
// | CountJobsOlderThan | BatchCountExpired; same explicit cutoff/query as purge. (The old AsyncJobStore receiver was deleted at H4; sessions gc routes through the BatchStore wrappers.) |
// | PurgeJobsOlderThan | BatchPurgeExpired; whole roots, controls cascade. (Old receiver deleted at H4.) |
// | HostLockStatus.String | Retained shared three-way verdict display. |
// | HostsDir | Retained shared host infrastructure. |
// | HostLockPath | Retained shared host infrastructure. |
// | retainHostLockUntilExit | Retained shutdown fence, not R9 lifecycle owner. |
// | releaseRetainedHostLocksForTest | Retained legacy test cleanup. |
// | markOwnHostID | Retained process-local registration fence. |
// | unmarkOwnHostID | Retained registration/test cleanup. |
// | IsOwnHostID | Retained; R9 skips own-host recovery. |
// | ProbeHostLock | Retained exclusive removal primitive, not sweep authority. |
// | ProbeHost | Retained guarded exclusive primitive, not sweep authority. |
// | ProbeHostLockShared | Retained OS dead-only proof. |
// | ProbeHostShared | BatchSharedHostProbe adapter; no retained lock. |
// | acquireFreshHostLock | Retained registration pathname verification. |
// | RegisterHost | Retained; R9 does not register execution hosts. |
// | HostIdentity.Close | Retained unchanged; R9 never invokes job-only deletion. |
// | RemoveDeadHostFile | Retained verified unlink, no R9 invocation. |
// | AsyncHostStoreFromConn | Retained legacy registration adapter. |
//
// Stop requests bind owner/host/root/node exactly. Invalid foreign-key targets
// receive typed nonpersisted rejection because the schema cannot store them.
// Pending and Cancelling are live targets. Unknown probes write nothing;
// confirmed-dead requests sweep instead of invoking a cancellation callback.
// The owning host retains one serial BatchControlPump per Runner incarnation.
// Its callback fences exact identities and deduplicates request IDs, including
// a replacement pump after callback success but answer-write failure. Callback
// success means accepted committed transition, not physical termination. No
// writer tx spans the callback, which may project through the same writer.
// Persisted answered requests never reach the callback again. Runner wiring,
// authentication, scheduling and the fatal exit boundary remain out of scope.

// BatchHostProbe is a three-way, dead-only authority. Host UUIDs never revive.
type BatchHostProbe func(string) (sessionhosts.HostLockStatus, error)

// BatchSharedHostProbe adapts the real hermetic OS shared-lock probe.
func BatchSharedHostProbe(dataDir string) BatchHostProbe {
	return func(host string) (sessionhosts.HostLockStatus, error) {
		return sessionhosts.ProbeHostShared(dataDir, host)
	}
}

type BatchRecoveryOutcome struct{ Interrupted, DeletedRoots, Notices, Controls int64 }

// BatchRecoverDeadHost returns announced work to durable history and deletes
// unannounced non-CLI reservations. CLI roots retain their durable identity
// even before an announcement; live CLI descendants are still interrupted.
// Each root commits separately; failures are returned without blocking other roots.
func (s *BatchStore) BatchRecoverDeadHost(ctx context.Context, host string, probe BatchHostProbe) (BatchRecoveryOutcome, error) {
	var total BatchRecoveryOutcome
	if host == "" || sessionhosts.IsOwnHostID(host) {
		return total, nil
	}
	if probe == nil {
		return total, errors.New("batch recovery: missing host probe")
	}
	status, err := probe(host)
	if status != sessionhosts.HostStatusDead || err != nil {
		return total, err
	}
	roots, err := db.New(s.writer).BatchRecoveryRoots(ctx)
	if err != nil {
		return total, err
	}
	var errs []error
	for _, r := range roots {
		if r.HostID != host {
			continue
		}
		out, err := s.batchRecoverRoot(ctx, r)
		if err != nil {
			errs = append(errs, fmt.Errorf("recover root %s: %w", r.RootID, err))
			continue
		}
		total.Interrupted += out.Interrupted
		total.DeletedRoots += out.DeletedRoots
		total.Notices += out.Notices
		total.Controls += out.Controls
	}
	return total, errors.Join(errs...)
}

func (s *BatchStore) batchRecoverRoot(ctx context.Context, listed db.BatchNode) (BatchRecoveryOutcome, error) {
	var out BatchRecoveryOutcome
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	r, err := q.BatchRecoveryElect(ctx, db.BatchRecoveryElectParams{RootID: listed.RootID, Owner: listed.OwnerSessionID, Host: listed.HostID})
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if r.Announced == 0 && r.OriginCli == 0 {
		out.DeletedRoots, err = q.DeleteBatchSubtree(ctx, db.DeleteBatchSubtreeParams{RootID: r.RootID, NodeID: r.NodeID, OwnerSessionID: r.OwnerSessionID, HostID: r.HostID})
	} else {
		nodes, e := q.ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: r.RootID, OwnerSessionID: r.OwnerSessionID})
		if e != nil {
			return out, e
		}
		live := false
		sequence := r.RootSequence
		for _, n := range nodes {
			live = live || batchStateLive(n.State)
			if n.ProjectionSequence > sequence {
				sequence = n.ProjectionSequence
			}
		}
		if !live {
			if sequence <= 0 {
				sequence = 1
			}
			out.Controls, err = q.BatchRecoveryCloseControls(ctx, db.BatchRecoveryCloseControlsParams{RootID: r.RootID, Owner: r.OwnerSessionID, Host: r.HostID, Sequence: sql.NullInt64{Int64: sequence, Valid: true}, Now: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}})
			if err != nil {
				return BatchRecoveryOutcome{}, err
			}
			if err = tx.Commit(); err != nil {
				return BatchRecoveryOutcome{}, err
			}
			return out, nil
		}
		sequence++
		now := time.Now().Unix()
		out.Interrupted, err = q.BatchRecoveryInterrupt(ctx, db.BatchRecoveryInterruptParams{RootID: r.RootID, Owner: r.OwnerSessionID, Host: r.HostID, Sequence: sequence, Now: now})
		if err != nil {
			return BatchRecoveryOutcome{}, err
		}
		// A terminal root keeps its outcome and projection sequence unchanged.
		if batchStateLive(r.State) {
			err = session.BatchRequireRow(q.AdvanceBatchRootSequence(ctx, db.AdvanceBatchRootSequenceParams{RootID: r.RootID, OwnerSessionID: r.OwnerSessionID, HostID: r.HostID, RunnerID: r.RunnerID, Sequence: sequence, UpdatedAt: now}))
			if err != nil {
				return BatchRecoveryOutcome{}, err
			}
		}
		out.Notices, err = q.BatchRecoveryNotice(ctx, db.BatchRecoveryNoticeParams{RootID: r.RootID, Owner: r.OwnerSessionID, Host: r.HostID, Sequence: sql.NullInt64{Int64: sequence, Valid: true}, Now: now})
		if err != nil {
			return BatchRecoveryOutcome{}, err
		}
		out.Controls, err = q.BatchRecoveryCloseControls(ctx, db.BatchRecoveryCloseControlsParams{RootID: r.RootID, Owner: r.OwnerSessionID, Host: r.HostID, Sequence: sql.NullInt64{Int64: sequence, Valid: true}, Now: sql.NullInt64{Int64: now, Valid: true}})
	}
	if err != nil {
		return BatchRecoveryOutcome{}, err
	}
	if err = tx.Commit(); err != nil {
		return BatchRecoveryOutcome{}, err
	}
	return out, nil
}

// BatchSweepDeadHosts discovers hosts from any live descendant or unannounced root.
// An owner filter selects hosts, not subsets of a host's recovery authority.
func (s *BatchStore) BatchSweepDeadHosts(ctx context.Context, owner string, probe BatchHostProbe) (map[string]BatchRecoveryOutcome, error) {
	roots, err := db.New(s.writer).BatchRecoveryRoots(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := map[string]BatchRecoveryOutcome{}
	var errs []error
	for _, r := range roots {
		if seen[r.HostID] || (owner != "" && r.OwnerSessionID != owner) {
			continue
		}
		seen[r.HostID] = true
		result, e := s.BatchRecoverDeadHost(ctx, r.HostID, probe)
		if e != nil {
			errs = append(errs, e)
		}
		if result != (BatchRecoveryOutcome{}) {
			out[r.HostID] = result
		}
	}
	return out, errors.Join(errs...)
}

// BatchRecoverOwnerScope discovers recovery hosts from this owner only.
func (s *BatchStore) BatchRecoverOwnerScope(ctx context.Context, owner string, probe BatchHostProbe) (map[string]BatchRecoveryOutcome, error) {
	if owner == "" {
		return map[string]BatchRecoveryOutcome{}, nil
	}
	return s.BatchSweepDeadHosts(ctx, owner, probe)
}
