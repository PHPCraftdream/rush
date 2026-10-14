// Batch rerun reconciliation (R10c, D-R10c-3/4): mirrors TruncateForRerun on
// node rows. ONE writer transaction deletes the target and its tail and
// reconciles every selected node against exactly the rows that transaction
// deleted: roots whose announce message was deleted void (plus the legacy
// arm: roots that never named an announce message void by the tail's
// tool_call_id), a done node whose notice message was deleted re-pends, and
// afterwards EVERY selected non-void node inside a voided root ends void —
// including a node the repend just re-pended (the void pass keys off the
// root_id, not the cleared message id). The
// IS NULL guard on the legacy arm keeps a provider-reused call id from
// ever reaching an incarnation that already acked (I5). Executors of voided
// nonterminal roots are stopped only after commit, from the Voided set
// returned here; every entry carries its exact root_id so the stop stays
// incarnation-scoped.
//
// D-R10c-live (R12-F round 4): live old incarnations (pending/starting/
// running/cancelling) are ALSO VOIDED IN THE SAME TRANSACTION. After the
// live-root SELECTs collect them, the caller writes BatchVoidRetiredRoot's
// synthetic select+void (notice_kind='root', notice_state='cancelled',
// notice_cause='stop', notice_sequence=1, notice_ordinal=0, delivery='void',
// wake=0) for exactly those root ids — CHECK-legal because the notice_* SET
// satisfies the notice_selected=1 branch. The write runs BEFORE
// BatchRerunVoidNodesOfVoidedRoots, so the already-selected descendants of
// now-void roots are voided by that existing pass keyed off root
// delivery='void'. Any SQL error rolls back the whole reconcile: the durable
// void is not best-effort here. The Voided set entries remain — they are for
// STOP ROUTING ONLY (the stop goroutine never writes delivery state).
package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// BatchRerunVoidedNode is one root incarnation the reconciliation voided, as
// it stood when voided. Stop routing must use RootID, never ToolCallID: a
// late completion/ack/stop of this incarnation can never reach a new one.
type BatchRerunVoidedNode struct {
	RootID, NodeID, ToolCallID string
	State                      string
	ChildSessionID, HostID     string
}

// BatchRerunTruncation is the committed outcome: the message rows actually
// deleted (target included) and the voided root incarnations.
type BatchRerunTruncation struct {
	Deleted []message.Message
	Voided  []BatchRerunVoidedNode
}

func batchRerunVoidedNode(n db.BatchNode) BatchRerunVoidedNode {
	return BatchRerunVoidedNode{RootID: n.RootID, NodeID: n.NodeID, ToolCallID: n.ToolCallID.String, State: n.State, ChildSessionID: n.ChildSessionID.String, HostID: n.HostID}
}

// BatchRerunReconcile runs the batch rerun truncation transaction; see the
// file doc. DeletedEvents publish only after commit. ErrRerunTargetGone rolls
// everything back.
func (s *BatchStore) BatchRerunReconcile(ctx context.Context, messages message.Service, p RerunTruncateParams) (BatchRerunTruncation, error) {
	if p.Owner == "" || p.TargetID == "" {
		return BatchRerunTruncation{}, errors.New("batch rerun: owner and target are required")
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchRerunTruncation{}, fmt.Errorf("batch rerun: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)
	now := time.Now().Unix()

	ids := make([]string, 0, len(p.TailIDs)+1)
	ids = append(ids, p.TargetID)
	ids = append(ids, p.TailIDs...)
	deleted, publish, err := messages.DeleteTx(ctx, tx, p.Owner, ids)
	if err != nil {
		return BatchRerunTruncation{}, fmt.Errorf("batch rerun: delete messages: %w", err)
	}
	targetDeleted := false
	deletedIDs := make([]string, 0, len(deleted))
	var tailToolCallIDs []string
	for _, m := range deleted {
		if m.ID == p.TargetID {
			targetDeleted = true
			continue
		}
		deletedIDs = append(deletedIDs, m.ID)
		for _, tc := range m.ToolCalls() {
			tailToolCallIDs = append(tailToolCallIDs, tc.ID)
		}
	}
	if !targetDeleted {
		return BatchRerunTruncation{}, ErrRerunTargetGone
	}

	// Roots first (announce arm, then the legacy arm), so the node passes
	// below can key off the voided roots (D-R10c-4).
	var voided []BatchRerunVoidedNode
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		rows, err := q.BatchRerunVoidRootsByAnnounceMessageIDs(ctx, db.BatchRerunVoidRootsByAnnounceMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: void roots by announce message: %w", err)
		}
		for _, r := range rows {
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
	}
	// Live old incarnations (D-R10c-live): collected first (below), then
	// voided in-tx by batchRerunVoidLiveRoots BEFORE
	// BatchRerunVoidNodesOfVoidedRoots, so the descendants pass sees the
	// roots' delivery='void' and voids their selected nodes too.
	var liveRootIDs []string
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		rows, err := q.BatchRerunLiveRootsByAnnounceMessageIDs(ctx, db.BatchRerunLiveRootsByAnnounceMessageIDsParams{
			Owner: p.Owner, MessageIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: live roots by announce message: %w", err)
		}
		for _, r := range rows {
			liveRootIDs = append(liveRootIDs, r.RootID)
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
	}
	// Legacy arm: never-acked roots only, so a reused tool_call_id cannot
	// reach a later incarnation that already acked (D-R10c-4).
	for _, chunk := range chunkStrings(tailToolCallIDs, rerunChunk) {
		rows, err := q.BatchRerunVoidRootsByToolCallIDs(ctx, db.BatchRerunVoidRootsByToolCallIDsParams{
			UpdatedAt: now, Owner: p.Owner, ToolCallIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: void roots by tool call: %w", err)
		}
		for _, r := range rows {
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
		rows, err = q.BatchRerunLiveRootsByToolCallIDs(ctx, db.BatchRerunLiveRootsByToolCallIDsParams{
			Owner: p.Owner, ToolCallIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: live roots by tool call: %w", err)
		}
		for _, r := range rows {
			liveRootIDs = append(liveRootIDs, r.RootID)
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
	}
	// Void the live roots IN THIS TRANSACTION (R12-F round 4, folds the
	// former post-commit BatchVoidRetiredRoot into reconcile). Only UPDATE
	// (1) — the synthetic select+void — is needed: a live root collected by
	// the SELECTs above cannot already carry notice_selected=1, and any
	// already-selected row of a now-void root (root itself included) is
	// covered by BatchRerunVoidNodesOfVoidedRoots further below, which is
	// why this write precedes it. Best-effort semantics are NOT acceptable
	// in-tx: any error rolls back the whole reconcile.
	if err = batchRerunVoidLiveRoots(ctx, tx, p.Owner, now, liveRootIDs); err != nil {
		return BatchRerunTruncation{}, err
	}
	// Re-pend: a node whose own notice message was deleted re-pends, unless
	// its ROOT was voided — the void pass below then voids it again, keyed by
	// root_id (D-R10c-3: a surviving root's node stays re-pended).
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		if _, err = q.BatchRerunRependNodesByNoticeMessageIDs(ctx, db.BatchRerunRependNodesByNoticeMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullStrings(chunk),
		}); err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: repend nodes: %w", err)
		}
	}
	// Voided roots ONCE, after the repend loop: void every selected, not-yet-
	// void node inside a voided root regardless of its (cleared) message id.
	if _, err = q.BatchRerunVoidNodesOfVoidedRoots(ctx, db.BatchRerunVoidNodesOfVoidedRootsParams{
		UpdatedAt: now, Owner: p.Owner,
	}); err != nil {
		return BatchRerunTruncation{}, fmt.Errorf("batch rerun: void nodes of voided roots: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return BatchRerunTruncation{}, fmt.Errorf("batch rerun: commit: %w", err)
	}
	publish()
	return BatchRerunTruncation{Deleted: deleted, Voided: voided}, nil
}

// batchRerunVoidLiveRoots voids the collected live root incarnations inside
// the reconcile transaction: one chunked UPDATE per id list, mirroring the
// former BatchVoidRetiredRoot UPDATE (1) exactly — synthetic select+void with
// the CHECK-legal notice identity (kind 'root', state 'cancelled', cause
// 'stop', sequence 1, ordinal 0), delivery='void', wake=0.
func batchRerunVoidLiveRoots(ctx context.Context, tx *sql.Tx, owner string, now int64, rootIDs []string) error {
	for _, chunk := range chunkStrings(rootIDs, rerunChunk) {
		query := `UPDATE batch_nodes SET
		notice_selected = 1, notice_kind = 'root', notice_state = 'cancelled',
		notice_cause = 'stop', notice_sequence = 1, notice_ordinal = 0,
		delivery = 'void', wake = 0, updated_at = ?
	WHERE owner_session_id = ? AND node_id = root_id
		AND notice_selected = 0
		AND state IN ('pending', 'starting', 'running', 'cancelling')
		AND root_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `)`
		args := make([]any, 0, len(chunk)+2)
		args = append(args, now, owner)
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("batch rerun: void live roots: %w", err)
		}
	}
	return nil
}

// batchRerunAddDistinct appends n unless a root_id entry already exists: a
// root matching both arms appears once in the stop-routing set.
func batchRerunAddDistinct(set []BatchRerunVoidedNode, n BatchRerunVoidedNode) []BatchRerunVoidedNode {
	for _, v := range set {
		if v.RootID == n.RootID {
			return set
		}
	}
	return append(set, n)
}

// BatchRerunVoidedRoots returns the exact incarnations a committed rerun
// voided. The durable void was already written in the truncation
// transaction; the agent's post-commit stop iterates these ONLY to stop
// executors, and never widens the key to a tool_call_id.
func BatchRerunVoidedRoots(res BatchRerunTruncation) []BatchRerunVoidedNode { return res.Voided }
