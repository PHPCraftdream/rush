// Read-only activity snapshots (R10c chunk 3, inert): one structured snapshot
// per root incarnation for the sessions jobs/why/show/list/audit readers.
// Facts, counts and the host verdict come from the R10a BatchLiveReader (its
// injectable host probe is reused, never bypassed); delivery/debt columns are
// joined from the same BatchLevelNodes rows. Structured, not text-parsed; no
// duplication of the R9/R10a readers.
package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
)

// BatchHistorySnapshot is the read-only state of one root incarnation:
// R10a live facts per node plus delivery/debt columns, the task counts, the
// host verdict for the root's host and the outstanding debt total (nodes
// with wake = 1, reacted = 0, delivery != 'void'). Disappeared mirrors the
// R10a level result: the root was deleted between capture and read. Complete
// mirrors level.Complete: an incomplete walk must never read as a finished
// or empty verdict (conservative-read rule).
type BatchHistorySnapshot struct {
	Owner, RootID string
	Root          BatchHistoryNodeView
	Nodes         []BatchHistoryNodeView
	Counts        BatchLiveCounts
	Debt          int64
	Host          HostLockStatus
	Disappeared   bool
	Complete      bool
}

func batchHistoryNodeView(fact BatchLiveNode, n db.BatchNode) BatchHistoryNodeView {
	return BatchHistoryNodeView{
		Fact: fact, Delivery: n.Delivery, Wake: n.Wake != 0,
		Reacted: n.Reacted != 0, MessageID: n.NoticeMessageID.String,
	}
}

func batchHistoryDebtRow(n db.BatchNode) bool {
	return n.NoticeSelected == 1 && n.Wake == 1 && n.Reacted == 0 && n.Delivery != "void"
}

// BatchHistorySnapshot reads one root incarnation. The reader supplies the
// source and the host probe; a nil reader or source is an error, not an
// empty snapshot (a zero value cannot prove absence).
func (s *BatchStore) BatchHistorySnapshot(ctx context.Context, reader *BatchLiveReader, owner, root string) (BatchHistorySnapshot, error) {
	if owner == "" || root == "" {
		return BatchHistorySnapshot{}, errors.New("batch snapshot: owner and root are required")
	}
	if reader == nil || reader.Source == nil {
		return BatchHistorySnapshot{}, errBatchReadUnavailable
	}
	rows, err := reader.Source.BatchLevelNodes(ctx, db.BatchLevelNodesParams{Owner: owner, RootID: root})
	if err != nil {
		return BatchHistorySnapshot{}, fmt.Errorf("batch snapshot: level nodes: %w", err)
	}
	level := reader.BatchLevel(ctx, BatchLiveRef{OwnerSessionID: owner, RootID: root}, "")
	if level.Err != nil {
		return BatchHistorySnapshot{}, fmt.Errorf("batch snapshot: level: %w", level.Err)
	}
	out := BatchHistorySnapshot{Owner: owner, RootID: root, Disappeared: level.Disappeared, Complete: level.Complete}
	byID := make(map[string]db.BatchNode, len(rows))
	for _, n := range rows {
		if n.OwnerSessionID == owner && n.RootID == root {
			byID[n.NodeID] = n
		}
	}
	// Pre-order from the level walk keeps parents before children and
	// siblings in ordinal order; rows the walk missed (cycle guard) keep
	// their raw facts out of the snapshot.
	for _, fact := range level.Subtree {
		n, ok := byID[fact.NodeID]
		if !ok {
			continue
		}
		view := batchHistoryNodeView(fact, n)
		out.Nodes = append(out.Nodes, view)
		out.Counts.add(n)
		if batchHistoryDebtRow(n) {
			out.Debt++
		}
		if n.NodeID == n.RootID {
			out.Root = view
		}
	}
	out.Host = level.Node.HostStatus
	return out, nil
}

// BatchHistoryRootViews is the oldest-first list of an owner's root
// incarnations, terminal included, each with its delivery/debt state and the
// reader's host verdict (sessions jobs/list input; D-R10c open decision:
// presentation mixing with legacy rows is a CLI decision at R12).
func (s *BatchStore) BatchHistoryRootViews(ctx context.Context, reader *BatchLiveReader, owner string) ([]BatchHistoryNodeView, error) {
	if owner == "" {
		return nil, errors.New("batch roots: owner is required")
	}
	if reader == nil || reader.Source == nil {
		return nil, errBatchReadUnavailable
	}
	roots, err := db.New(s.writer).BatchHistoryRootsForOwner(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("batch roots: list: %w", err)
	}
	out := make([]BatchHistoryNodeView, 0, len(roots))
	for _, r := range roots {
		out = append(out, BatchHistoryNodeView{
			Fact:     batchLiveFact(r, r.ToolCallID.String, 0, reader.hostStatus(r.HostID)),
			Delivery: r.Delivery, Wake: r.Wake != 0, Reacted: r.Reacted != 0,
			MessageID: r.NoticeMessageID.String,
		})
	}
	return out, nil
}
