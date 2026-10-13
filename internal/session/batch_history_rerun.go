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
// D-R10c-live: the batch_nodes CHECK constraint forbids delivery='void' on a
// notice_selected=0 row, and ordinary live roots (pending/starting/running/
// cancelling) have no selected terminal notice — a void WRITE to them is a
// schema violation. Live old incarnations are therefore collected with pure
// SELECTs (BatchRerunLiveRootsBy{AnnounceMessage,ToolCall}IDs) and included
// in the Voided set with their exact root_id, retiring them via post-commit
// stop routing instead of delivery (mirrors legacy TruncateForRerun, which
// voids running rows regardless of state). Entries are kept distinct by
// root_id: a root matching both arms appears once.
package session

import (
	"context"
	"errors"
	"fmt"
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
	// Live old incarnations (D-R10c-live): a void WRITE is impossible for
	// unselected rows (schema CHECK), so they retire via stop routing only.
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		rows, err := q.BatchRerunLiveRootsByAnnounceMessageIDs(ctx, db.BatchRerunLiveRootsByAnnounceMessageIDsParams{
			Owner: p.Owner, MessageIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: live roots by announce message: %w", err)
		}
		for _, r := range rows {
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
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
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
// voided; the agent's post-commit stop iterates these and never widens the
// key to a tool_call_id.
func BatchRerunVoidedRoots(res BatchRerunTruncation) []BatchRerunVoidedNode { return res.Voided }
