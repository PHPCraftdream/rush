package tasktree

import "strconv"

func ValidateLimits(l Limits) error {
	if l.MaxNodes <= 0 || l.MaxDepth <= 0 || l.MaxTitleBytes <= 0 || l.MaxReasonBytes <= 0 || l.MaxTombstones <= 0 || l.MaxReceipts <= 0 {
		return kernelProblem(CodeInvalidInput, "all task tree limits must be positive")
	}
	return nil
}

func EmptySnapshot() Snapshot {
	return Snapshot{
		SchemaVersion: SchemaVersion, RootID: RootID, NextID: 1,
		Nodes:      map[NodeID]Node{RootID: {ID: RootID, Kind: KindGroup, Title: "Tasks"}},
		Tombstones: make(map[NodeID]Tombstone), TitleGuards: []TitleGuard{},
	}
}

func CloneSnapshot(s Snapshot) Snapshot {
	out := s
	if s.Nodes != nil {
		out.Nodes = make(map[NodeID]Node, len(s.Nodes))
		for id, n := range s.Nodes {
			if n.Children != nil {
				n.Children = append(make([]NodeID, 0, len(n.Children)), n.Children...)
			}
			out.Nodes[id] = n
		}
	}
	if s.Tombstones != nil {
		out.Tombstones = make(map[NodeID]Tombstone, len(s.Tombstones))
		for id, tomb := range s.Tombstones {
			out.Tombstones[id] = tomb
		}
	}
	if s.TitleGuards != nil {
		out.TitleGuards = append(make([]TitleGuard, 0, len(s.TitleGuards)), s.TitleGuards...)
	}
	return out
}

func kernelCounter(id NodeID) (uint64, bool) {
	if len(id) < 2 || id[0] != 'n' {
		return 0, false
	}
	n, err := strconv.ParseUint(string(id[1:]), 36, 64)
	return n, err == nil && kernelID(n) == id
}

func ValidateSnapshot(s Snapshot, l Limits) error {
	if err := ValidateLimits(l); err != nil {
		return err
	}
	invalid := func(message string) error { return kernelProblem(CodeInvalidSnapshot, message) }
	if s.SchemaVersion != SchemaVersion || s.RootID != RootID || s.NextID == 0 {
		return invalid("unsupported schema, root ID, or next counter")
	}
	if len(s.Nodes) > l.MaxNodes || len(s.Tombstones) > l.MaxTombstones || len(s.TitleGuards) > l.MaxTombstones {
		return kernelProblem(CodeLimitExceeded, "snapshot exceeds live node or retained history limits")
	}
	root, ok := s.Nodes[RootID]
	if !ok || root.ID != RootID || root.ParentID != "" || root.Kind != KindGroup || root.Title != "Tasks" {
		return invalid("root must be the intrinsic parentless Tasks group")
	}
	if !s.Initialized && (len(s.Nodes) != 1 || len(root.Children) != 0 || len(s.Tombstones) != 0 || s.NextID != 1) {
		return invalid("uninitialized snapshot must be root-only with no children or tombstones and next counter 1; valid title guards are permitted")
	}
	maxCounter := uint64(0)
	active := 0
	for id, n := range s.Nodes {
		counter, canonical := kernelCounter(id)
		if !canonical || n.ID != id || counter >= s.NextID {
			return invalid("live node has a noncanonical ID, mismatched key, or reusable counter")
		}
		if counter > maxCounter {
			maxCounter = counter
		}
		if id != RootID {
			if n.ParentID == "" {
				return invalid("non-root node has no parent")
			}
			if err := kernelLabel(n.Title, l.MaxTitleBytes); err != nil {
				return invalid("node title is blank or exceeds byte limit")
			}
		}
		switch n.Kind {
		case KindGroup:
			if n.Status != "" || n.Reason != "" || n.ActiveForm != "" {
				return invalid("groups cannot carry task state, reason, or active_form")
			}
		case KindTask:
			if len(n.Children) != 0 {
				return invalid("tasks cannot have children")
			}
			switch n.Status {
			case Pending, InProgress, Completed:
				if n.Reason != "" {
					return invalid("only blocked or abandoned tasks may have reasons")
				}
			case Blocked, Abandoned:
				if err := kernelLabel(n.Reason, l.MaxReasonBytes); err != nil {
					return invalid("blocked or abandoned task requires a bounded nonblank reason")
				}
			default:
				return invalid("unknown task status")
			}
			if n.Status == InProgress {
				active++
			}
		default:
			return invalid("unknown node kind")
		}
	}
	if active > 1 {
		return invalid("snapshot contains multiple focus tasks")
	}
	for id, tomb := range s.Tombstones {
		counter, canonical := kernelCounter(id)
		if !canonical || counter == 0 || counter >= s.NextID || tomb.ID != id {
			return invalid("tombstone has an invalid ID or counter")
		}
		if _, live := s.Nodes[id]; live {
			return invalid("live and removed IDs overlap")
		}
		if tomb.Kind != KindTask && tomb.Kind != KindGroup {
			return invalid("tombstone has invalid kind")
		}
		if err := kernelLabel(tomb.Title, l.MaxTitleBytes); err != nil {
			return invalid("tombstone title is invalid")
		}
		if err := kernelActor(tomb.Actor); err != nil || tomb.Actor.Kind != ActorOperator {
			return invalid("tombstones must record a valid operator")
		}
		if counter > maxCounter {
			maxCounter = counter
		}
	}
	if s.NextID <= maxCounter {
		return invalid("next counter must exceed every live and removed ID counter")
	}
	guards := make(map[TitleGuard]bool, len(s.TitleGuards))
	for _, guard := range s.TitleGuards {
		if guard.Kind != KindTask && guard.Kind != KindGroup {
			return invalid("title guard has invalid kind")
		}
		if err := kernelLabel(guard.Title, l.MaxTitleBytes); err != nil {
			return invalid("title guard label is invalid")
		}
		if guards[guard] {
			return invalid("duplicate title guard")
		}
		guards[guard] = true
	}
	incoming := make(map[NodeID]bool, len(s.Nodes))
	for id, n := range s.Nodes {
		for _, child := range n.Children {
			cn, found := s.Nodes[child]
			if !found || child == RootID || cn.ParentID != id || incoming[child] {
				return invalid("unknown/repeated child or parent-child mismatch")
			}
			incoming[child] = true
		}
	}
	for id := range s.Nodes {
		if id != RootID && !incoming[id] {
			return invalid("orphan node is not listed by its parent")
		}
	}
	type item struct {
		id    NodeID
		depth int
	}
	stack := []item{{RootID, 0}}
	seen := make(map[NodeID]bool, len(s.Nodes))
	for len(stack) != 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[x.id] {
			return invalid("cycle or repeated node in traversal")
		}
		seen[x.id] = true
		if x.depth > l.MaxDepth {
			return kernelProblem(CodeLimitExceeded, "snapshot exceeds maximum depth")
		}
		for _, child := range s.Nodes[x.id].Children {
			stack = append(stack, item{child, x.depth + 1})
		}
	}
	if len(seen) != len(s.Nodes) {
		return invalid("unreachable cycle or orphan component")
	}
	return nil
}

func Restore(s Snapshot, l Limits) (*Tree, error) {
	if err := ValidateSnapshot(s, l); err != nil {
		return nil, err
	}
	return kernelOwnedTree(CloneSnapshot(s), l), nil
}

func kernelOwnedTree(s Snapshot, l Limits) *Tree {
	t := &Tree{state: s, limits: l, valid: true, guards: make(map[TitleGuard]bool, len(s.TitleGuards))}
	for _, g := range s.TitleGuards {
		t.guards[g] = true
	}
	for id, n := range s.Nodes {
		if n.Status == InProgress {
			t.active = id
		}
	}
	return t
}

func (t *Tree) Snapshot() Snapshot {
	t.requireOwned()
	return CloneSnapshot(t.state)
}

func (t *Tree) takeSnapshot() Snapshot {
	t.requireOwned()
	state := t.state
	t.state = Snapshot{}
	t.order = nil
	t.guards = nil
	t.active = ""
	t.valid = false
	return state
}
