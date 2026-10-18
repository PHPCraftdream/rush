package session

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync/atomic"

	"github.com/PHPCraftdream/rush/internal/db"
)

const (
	batchReadChunk = 500
	batchReadDepth = 16
)

// batchLiveServedSeam is a test-only hook invoked between the root list and
// the per-root node reads of batchLiveNodesForOwnersServed; nil in production
// (the readTurnFactsGateSeam precedent in internal/agent).
var batchLiveServedSeam atomic.Pointer[func()]

// BatchLiveForSessions shares BFS reads and host probes across a session forest.
// Links come only from nodes belonging to live roots, never sessions.parent_id.
// Dead hosts remain live until recovery commits; Unknown makes reads incomplete.
func (r *BatchLiveReader) BatchLiveForSessions(ctx context.Context, sessions []string) map[string]BatchLiveResult {
	if r == nil || r.Source == nil || ctx == nil {
		return batchLiveResultsUnavailable(sessions)
	}
	return r.batchLiveForSessions(ctx, sessions, func(chunk []string) ([]db.BatchNode, error) {
		if q, ok := r.Source.(*db.Queries); ok {
			// The join query (BatchLiveNodesForOwners) scans batch_nodes
			// as the table grows: its owner/state predicates match no
			// index (every owner-leading index is partial on terms the
			// query does not assert). The served two-step read below is
			// index-served and per-owner bounded.
			return batchLiveNodesForOwnersServed(ctx, q, chunk)
		}
		return r.Source.BatchLiveNodesForOwners(ctx, chunk)
	})
}

// BatchLiveForSessionsTx is BatchLiveForSessions with every fetch running
// inside ONE read-only transaction on dbtx: roots and their nodes come from
// a single SQLite snapshot (a WAL deferred read transaction), so a root that
// settles or is purged mid-read keeps its rows and a root submitted mid-read
// stays invisible — exactly the old single-statement join's semantics. A nil
// dbtx falls back to the plain read.
func (r *BatchLiveReader) BatchLiveForSessionsTx(ctx context.Context, sessions []string, dbtx db.DBTX) map[string]BatchLiveResult {
	if r == nil || r.Source == nil || ctx == nil || dbtx == nil {
		return r.BatchLiveForSessions(ctx, sessions)
	}
	tx, err := dbtx.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return batchLiveResultsUnavailable(sessions, err)
	}
	defer func() { _ = tx.Rollback() }()
	return r.batchLiveForSessions(ctx, sessions, func(chunk []string) ([]db.BatchNode, error) {
		return batchLiveNodesForOwnersServed(ctx, db.New(tx), chunk)
	})
}

func batchLiveResultsUnavailable(sessions []string, errs ...error) map[string]BatchLiveResult {
	out := make(map[string]BatchLiveResult, len(sessions))
	var err error
	for _, e := range errs {
		err = errors.Join(err, e)
	}
	if err == nil {
		err = errBatchReadUnavailable
	}
	for _, owner := range sessions {
		if owner == "" {
			continue
		}
		out[owner] = BatchLiveResult{Err: err}
	}
	return out
}

func (r *BatchLiveReader) batchLiveForSessions(ctx context.Context, sessions []string, fetch func([]string) ([]db.BatchNode, error)) map[string]BatchLiveResult {
	out := make(map[string]BatchLiveResult)
	var frontier []string
	for _, owner := range sessions {
		if owner == "" {
			continue
		}
		if _, ok := out[owner]; ok {
			continue
		}
		out[owner] = BatchLiveResult{}
		frontier = append(frontier, owner)
	}
	rowsByOwner := make(map[string][]db.BatchNode)
	failures := make(map[string]error)
	fetched := make(map[string]bool)
	hosts := make(map[string]HostLockStatus)
	statusOf := func(host string) HostLockStatus {
		if st, ok := hosts[host]; ok {
			return st
		}
		st := r.hostStatus(host)
		hosts[host] = st
		return st
	}
	for depth := 0; depth <= batchReadDepth && len(frontier) > 0; depth++ {
		var todo, next []string
		for _, owner := range frontier {
			if !fetched[owner] {
				fetched[owner] = true
				todo = append(todo, owner)
			}
		}
		for start := 0; start < len(todo); start += batchReadChunk {
			chunk := todo[start:min(start+batchReadChunk, len(todo))]
			err := ctx.Err()
			var rows []db.BatchNode
			if err == nil {
				rows, err = fetch(chunk)
			}
			if err != nil {
				for _, owner := range chunk {
					failures[owner] = err
				}
				continue
			}
			for _, n := range rows {
				rowsByOwner[n.OwnerSessionID] = append(rowsByOwner[n.OwnerSessionID], n)
				statusOf(n.HostID)
				if n.ChildSessionID.Valid && n.ChildSessionID.String != "" {
					next = append(next, n.ChildSessionID.String)
				}
			}
		}
		frontier = next
	}
	for owner := range out {
		out[owner] = batchLiveReplay(owner, rowsByOwner, failures, fetched, statusOf)
	}
	return out
}

func batchLiveReplay(owner string, rows map[string][]db.BatchNode, failures map[string]error, fetched map[string]bool, statusOf func(string) HostLockStatus) BatchLiveResult {
	v := BatchLiveResult{Complete: true, OwnComplete: true}
	visited := map[string]bool{owner: true}
	frontier := []string{owner}
	for depth := 0; depth <= batchReadDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, session := range frontier {
			err := failures[session]
			if !fetched[session] && err == nil {
				err = errBatchReadUnavailable
			}
			if err != nil {
				v.Complete = false
				v.Err = errors.Join(v.Err, err)
				if depth == 0 {
					v.OwnComplete = false
				}
				continue
			}
			nodes := rows[session]
			roots := make(map[string]db.BatchNode)
			counts := make(map[string]BatchLiveCounts)
			for _, n := range nodes {
				c := counts[n.RootID]
				c.add(n)
				counts[n.RootID] = c
				if n.NodeID == n.RootID {
					roots[n.RootID] = n
				}
			}
			for _, n := range nodes {
				root, ok := roots[n.RootID]
				if !ok {
					v.Complete = false
					if depth == 0 {
						v.OwnComplete = false
					}
					continue
				}
				st := statusOf(n.HostID)
				if st == HostStatusUnknown {
					v.Complete = false
					if depth == 0 {
						v.OwnComplete = false
					}
				}
				fact := batchLiveFact(n, root.ToolCallID.String, depth, st)
				if n.NodeID == n.RootID {
					fact.Counts = counts[n.RootID]
					v.Tree = append(v.Tree, fact)
					if depth == 0 {
						v.Own = append(v.Own, fact)
					}
				}
				v.Counts.add(n)
				child := n.ChildSessionID.String
				if n.ChildSessionID.Valid && child != "" && !visited[child] {
					visited[child] = true
					next = append(next, child)
					v.Descendants = append(v.Descendants, fact)
				}
			}
		}
		frontier = next
	}
	if len(frontier) > 0 {
		v.Complete = false
	}
	return v
}

// batchLiveNodesForOwnersServed is the index-served replacement for
// BatchLiveNodesForOwners: it lists each owner's live roots through
// ListLiveBatchRootsForOwner (idx_batch_nodes_owner_roots, already ordered by
// created_at, root_id) and each root's nodes through ListBatchNodes
// (idx_batch_nodes_parent), so no statement scans batch_nodes. The returned
// rows and their order match the join query's ORDER BY (r.created_at,
// r.root_id, n.ordinal, n.node_id) exactly: nodes within one root are sorted
// by (ordinal, node_id) and roots are visited in the root list's order.
func batchLiveNodesForOwnersServed(ctx context.Context, q *db.Queries, owners []string) ([]db.BatchNode, error) {
	var out []db.BatchNode
	for _, owner := range owners {
		roots, err := q.ListLiveBatchRootsForOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
		if seam := batchLiveServedSeam.Load(); seam != nil {
			(*seam)()
		}
		for _, root := range roots {
			nodes, err := q.ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: root.RootID, OwnerSessionID: owner})
			if err != nil {
				return nil, err
			}
			sort.SliceStable(nodes, func(i, j int) bool {
				if nodes[i].Ordinal != nodes[j].Ordinal {
					return nodes[i].Ordinal < nodes[j].Ordinal
				}
				return nodes[i].NodeID < nodes[j].NodeID
			})
			out = append(out, nodes...)
		}
	}
	return out, nil
}

// BatchLiveForSessionsTx runs the reader's served read inside one read-only
// transaction on this store's writer: the same single-connection writer the
// live reads already share, so roots and their nodes are one snapshot.
func (s *BatchStore) BatchLiveForSessionsTx(ctx context.Context, reader *BatchLiveReader, sessions []string) map[string]BatchLiveResult {
	if s == nil {
		return reader.BatchLiveForSessions(ctx, sessions)
	}
	return reader.BatchLiveForSessionsTx(ctx, sessions, s.writer)
}
