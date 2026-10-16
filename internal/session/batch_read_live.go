package session

import (
	"context"
	"errors"

	"github.com/PHPCraftdream/rush/internal/db"
)

const (
	batchReadChunk = 500
	batchReadDepth = 16
)

// BatchLiveForSessions shares BFS reads and host probes across a session forest.
// Links come only from nodes belonging to live roots, never sessions.parent_id.
// Dead hosts remain live until recovery commits; Unknown makes reads incomplete.
func (r *BatchLiveReader) BatchLiveForSessions(ctx context.Context, sessions []string) map[string]BatchLiveResult {
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
	if r == nil || r.Source == nil || ctx == nil {
		for owner := range out {
			out[owner] = BatchLiveResult{Err: errBatchReadUnavailable}
		}
		return out
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
				rows, err = r.Source.BatchLiveNodesForOwners(ctx, chunk)
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
