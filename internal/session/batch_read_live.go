package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// SetBatchLiveServedSeamForTest arms the served-read seam for cross-package
// tests (internal/agent) and returns the restore func. Production never
// calls it; the seam stays nil there.
func SetBatchLiveServedSeamForTest(hook func()) (restore func()) {
	if hook == nil {
		batchLiveServedSeam.Store(nil)
		return func() {}
	}
	h := hook
	batchLiveServedSeam.Store(&h)
	return func() { batchLiveServedSeam.Store(nil) }
}

// BatchLiveTxStarter opens the read-only transactions the batch live read
// runs inside. Satisfied by *sql.DB; *sql.Tx cannot begin nested
// transactions.
type BatchLiveTxStarter interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// BatchLiveForSessions shares BFS reads and host probes across a session forest.
// Links come only from nodes belonging to live roots, never sessions.parent_id.
// Dead hosts remain live until recovery commits; Unknown makes reads incomplete.
//
// With a non-nil TxStarter every fetch runs inside ONE read-only transaction
// (a single SQLite snapshot: a root that settles or is purged mid-read keeps
// its rows, a root submitted mid-read stays invisible). A vanished listed root
// never silently reads as complete: on the first attempt only, such a snapshot
// is retried once on a fresh transaction. A nil TxStarter falls back to the
// single-statement join — consistent by construction, scan-class cost.
func (r *BatchLiveReader) BatchLiveForSessions(ctx context.Context, sessions []string) map[string]BatchLiveResult {
	if r == nil || r.Source == nil || ctx == nil {
		return batchLiveResultsUnavailable(sessions)
	}
	if r.TxStarter == nil {
		return r.batchLiveForSessions(ctx, sessions, func(chunk []string) ([]db.BatchNode, error) {
			return r.Source.BatchLiveNodesForOwners(ctx, chunk)
		})
	}
	for attempt := 0; ; attempt++ {
		tx, err := r.TxStarter.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return batchLiveResultsUnavailable(sessions, err)
		}
		out := r.batchLiveForSessions(ctx, sessions, func(chunk []string) ([]db.BatchNode, error) {
			return batchLiveNodesForOwnersServed(ctx, db.New(tx), chunk)
		})
		_ = tx.Rollback()
		if attempt == 0 {
			vanished := false
			for _, res := range out {
				if res.Err != nil && errors.Is(res.Err, errBatchLiveRootVanished) {
					vanished = true
					break
				}
			}
			if vanished {
				continue
			}
		}
		return out
	}
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

// errBatchLiveRootVanished reports a listed live root whose rows vanished from
// the read's snapshot.
var errBatchLiveRootVanished = errors.New("batch live read: listed root vanished mid-read")

type batchLiveRootEntry struct {
	owner string
	root  db.BatchNode
}

// batchLiveNodesForOwnersServed is the index-served replacement for
// BatchLiveNodesForOwners: it lists each owner's live roots through
// ListLiveBatchRootsForOwner (idx_batch_nodes_owner_roots) and each root's
// nodes through ListBatchNodes (idx_batch_nodes_parent), so no statement
// scans batch_nodes. Ordering contract, matching the join query's ORDER BY
// (r.created_at, r.root_id, n.ordinal, n.node_id) EXACTLY across owners:
// roots are visited in GLOBAL (created_at, root_id) order — collected from
// all owners first and sorted, not per-owner — then nodes within one root in
// (ordinal, node_id) order. The seam fires once, after all root lists and
// before the first node read. A live root always contains its own root row;
// zero rows therefore mean the root's rows vanished from this snapshot, which
// is reported as errBatchLiveRootVanished instead of reading as complete.
func batchLiveNodesForOwnersServed(ctx context.Context, q *db.Queries, owners []string) ([]db.BatchNode, error) {
	var entries []batchLiveRootEntry
	for _, owner := range owners {
		roots, err := q.ListLiveBatchRootsForOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, root := range roots {
			entries = append(entries, batchLiveRootEntry{owner: owner, root: root})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].root.CreatedAt != entries[j].root.CreatedAt {
			return entries[i].root.CreatedAt < entries[j].root.CreatedAt
		}
		return entries[i].root.RootID < entries[j].root.RootID
	})
	if seam := batchLiveServedSeam.Load(); seam != nil {
		(*seam)()
	}
	var out []db.BatchNode
	for _, entry := range entries {
		nodes, err := q.ListBatchNodes(ctx, db.ListBatchNodesParams{RootID: entry.root.RootID, OwnerSessionID: entry.owner})
		if err != nil {
			return nil, err
		}
		if len(nodes) == 0 {
			return nil, fmt.Errorf("%w: root %s", errBatchLiveRootVanished, entry.root.RootID)
		}
		sort.SliceStable(nodes, func(i, j int) bool {
			if nodes[i].Ordinal != nodes[j].Ordinal {
				return nodes[i].Ordinal < nodes[j].Ordinal
			}
			return nodes[i].NodeID < nodes[j].NodeID
		})
		out = append(out, nodes...)
	}
	return out, nil
}
