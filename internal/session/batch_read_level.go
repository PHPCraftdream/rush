package session

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/db"
)

// BatchLevelResult contains the addressed node, ordinal-sorted direct children,
// and its pre-order subtree at one SQL statement's snapshot. Counts count tasks.
// Disappeared is never Finished: deletion is not a confirmed termination fact.
type BatchLevelResult struct {
	Ref                             BatchLiveRef
	Node                            BatchLiveNode
	Children, Subtree               []BatchLiveNode
	Counts                          BatchLiveCounts
	Complete, Disappeared, Finished bool
	Err                             error
}

func (v BatchLevelResult) HasLive() bool { return !v.Complete || v.Disappeared || !v.Finished }

// BatchLevel reads a root or nested node without inferring settlement from
// control answers, selected notices, announcement, delivery or host death.
func (r *BatchLiveReader) BatchLevel(ctx context.Context, ref BatchLiveRef, nodeID string) BatchLevelResult {
	v := BatchLevelResult{Ref: ref}
	if r == nil || r.Source == nil || ctx == nil || ref.OwnerSessionID == "" || ref.RootID == "" {
		v.Err = errBatchReadUnavailable
		return v
	}
	if nodeID == "" {
		nodeID = ref.RootID
	}
	rows, err := r.Source.BatchLevelNodes(ctx, db.BatchLevelNodesParams{Owner: ref.OwnerSessionID, RootID: ref.RootID})
	if err != nil {
		v.Err = err
		return v
	}
	v.Complete = true
	byID := make(map[string]db.BatchNode)
	children := make(map[string][]string)
	hosts := make(map[string]HostLockStatus)
	for _, n := range rows {
		if n.OwnerSessionID != ref.OwnerSessionID || n.RootID != ref.RootID {
			v.Complete = false
			continue
		}
		byID[n.NodeID] = n
		if n.ParentID.Valid {
			children[n.ParentID.String] = append(children[n.ParentID.String], n.NodeID)
		}
	}
	root, rootOK := byID[ref.RootID]
	node, nodeOK := byID[nodeID]
	if !rootOK || !nodeOK {
		v.Disappeared = true
		return v
	}
	// SQL orders every sibling set by ordinal, not by lexical derived node ID.
	facts := make(map[string]BatchLiveNode)
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var walk func(string, int) BatchLiveCounts
	walk = func(id string, depth int) BatchLiveCounts {
		var counts BatchLiveCounts
		if visiting[id] || visited[id] || depth > batchReadDepth {
			v.Complete = false
			return counts
		}
		n, ok := byID[id]
		if !ok {
			v.Complete = false
			return counts
		}
		visiting[id] = true
		visited[id] = true
		st, ok := hosts[n.HostID]
		if !ok {
			st = r.hostStatus(n.HostID)
			hosts[n.HostID] = st
		}
		if st == HostStatusUnknown {
			v.Complete = false
		}
		fact := batchLiveFact(n, root.ToolCallID.String, depth, st)
		pos := len(v.Subtree)
		v.Subtree = append(v.Subtree, fact)
		counts.add(n)
		for _, child := range children[id] {
			c := walk(child, depth+1)
			counts.Pending += c.Pending
			counts.Starting += c.Starting
			counts.Running += c.Running
			counts.Cancelling += c.Cancelling
			counts.Active += c.Active
			counts.Completed += c.Completed
			counts.Failed += c.Failed
			counts.Cancelled += c.Cancelled
			counts.TimedOut += c.TimedOut
			counts.Interrupted += c.Interrupted
			counts.Skipped += c.Skipped
		}
		visiting[id] = false
		fact.Counts = counts
		facts[id] = fact
		v.Subtree[pos] = fact
		return counts
	}
	v.Counts = walk(nodeID, 0)
	v.Node = facts[nodeID]
	for _, id := range children[nodeID] {
		if fact, ok := facts[id]; ok {
			v.Children = append(v.Children, fact)
		}
	}
	if nodeID == ref.RootID && len(visited) != len(byID) {
		v.Complete = false
	}
	v.Finished = v.Complete && batchReadTerminal(node.State)
	return v
}

// BatchCapturedRoots privately retains entry identities. Refs returns a copy;
// later roots cannot be appended by callers or by subsequent level reads.
type BatchCapturedRoots struct {
	refs     []BatchLiveRef
	complete bool
	err      error
}

func (v BatchCapturedRoots) Refs() []BatchLiveRef { return append([]BatchLiveRef(nil), v.refs...) }
func (v BatchCapturedRoots) Complete() bool       { return v.complete }
func (v BatchCapturedRoots) HasLive() bool        { return !v.complete || len(v.refs) > 0 }

// BatchCapturedLevels observes exactly the captured incarnations. No Done,
// waiting, await-unit selection or durable debt is implemented here.
type BatchCapturedLevels struct {
	Roots    []BatchLevelResult
	Complete bool
	Err      error
}

func (v BatchCapturedLevels) HasLive() bool {
	if !v.Complete {
		return true
	}
	for _, root := range v.Roots {
		if root.HasLive() {
			return true
		}
	}
	return false
}

func (r *BatchLiveReader) BatchCaptured(ctx context.Context, sessions []string) BatchCapturedRoots {
	reads := r.BatchLiveForSessions(ctx, sessions)
	v := BatchCapturedRoots{complete: ctx != nil && r != nil && r.Source != nil}
	seen := make(map[BatchLiveRef]bool)
	for _, session := range sessions {
		if session == "" {
			v.complete = false
			v.err = errBatchReadUnavailable
			continue
		}
		read, ok := reads[session]
		if !ok || !read.Complete {
			v.complete = false
		}
		if read.Err != nil {
			v.err = read.Err
		}
		for _, root := range read.Tree {
			if !seen[root.Ref] {
				seen[root.Ref] = true
				v.refs = append(v.refs, root.Ref)
			}
		}
	}
	return v
}

// BatchCapturedLevel rereads levels, including later Add children, without
// enumerating any newly submitted root. Incomplete capture stays incomplete.
func (r *BatchLiveReader) BatchCapturedLevel(ctx context.Context, captured BatchCapturedRoots) BatchCapturedLevels {
	v := BatchCapturedLevels{Complete: captured.complete, Err: captured.err}
	if ctx == nil || r == nil || r.Source == nil {
		v.Complete = false
		v.Err = errBatchReadUnavailable
	} else if err := ctx.Err(); err != nil {
		v.Complete = false
		v.Err = err
	}
	for _, ref := range captured.refs {
		level := r.BatchLevel(ctx, ref, ref.RootID)
		v.Roots = append(v.Roots, level)
		if !level.Complete {
			v.Complete = false
		}
		if level.Err != nil {
			v.Err = level.Err
		}
	}
	return v
}
