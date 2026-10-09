package tasktree

import "strings"

func (t *Tree) planAdd(p *kernelPlan, actor Actor, c Command) error {
	parent := c.ParentID
	if parent == "" {
		parent = RootID
	}
	n, err := t.node(parent)
	if err != nil {
		return err
	}
	if n.Kind != KindGroup {
		return kernelProblem(CodeInvalidTargetKind, "add parent must be a group")
	}
	type item struct {
		draft  *Draft
		parent NodeID
		depth  int
	}
	stack := make([]item, 0, len(c.Drafts))
	depth := t.depth(parent) + 1
	for i := len(c.Drafts) - 1; i >= 0; i-- {
		stack = append(stack, item{&c.Drafts[i], parent, depth})
	}
	for len(stack) != 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		d := x.draft
		if d.Kind != KindTask && d.Kind != KindGroup {
			return kernelProblem(CodeInvalidInput, "draft kind must be task or group")
		}
		if err := kernelLabel(d.Title, t.limits.MaxTitleBytes); err != nil {
			return err
		}
		if (d.Kind == KindTask && len(d.Children) != 0) || (d.Kind == KindGroup && d.ActiveForm != "") {
			return kernelProblem(CodeInvalidInput, "tasks cannot have children and groups cannot have active_form")
		}
		if x.depth > t.limits.MaxDepth || len(p.created) >= t.limits.MaxNodes-len(t.state.Nodes) {
			return kernelProblem(CodeLimitExceeded, "addition exceeds node or depth limit")
		}
		g := TitleGuard{Kind: d.Kind, Title: d.Title}
		if t.guards[g] && actor.Kind == ActorAgent {
			return kernelProblem(CodeRemovedByOperator, "operator-deleted label requires explicit operator restoration")
		}
		if actor.Kind == ActorOperator && t.guards[g] {
			if p.clearGuards == nil {
				p.clearGuards = make(map[TitleGuard]bool)
			}
			p.clearGuards[g] = true
		}
		if p.next == ^uint64(0) {
			return kernelProblem(CodeLimitExceeded, "node ID counter exhausted")
		}
		id := kernelID(p.next)
		p.next++
		created := Node{ID: id, ParentID: x.parent, Kind: d.Kind, Title: d.Title, ActiveForm: d.ActiveForm}
		if d.Kind == KindTask {
			created.Status = Pending
			p.advance = true
		}
		p.writes[id] = created
		if p.created == nil {
			p.created = make(map[NodeID]bool)
		}
		p.created[id] = true
		pn := p.get(t, x.parent)
		if _, owned := p.writes[x.parent]; !owned {
			pn.Children = append([]NodeID(nil), pn.Children...)
		}
		pn.Children = append(pn.Children, id)
		p.writes[x.parent] = pn
		for i := len(d.Children) - 1; i >= 0; i-- {
			stack = append(stack, item{&d.Children[i], id, x.depth + 1})
		}
	}
	p.initialize = c.Op == OpInit
	p.structure = true
	return nil
}

func (t *Tree) planStatus(p *kernelPlan, n Node, c Command) error {
	if c.Op == OpBlock || c.Op == OpDrop {
		if len(c.Reason) > t.limits.MaxReasonBytes {
			return kernelProblem(CodeLimitExceeded, "reason exceeds byte limit")
		}
		if strings.TrimSpace(c.Reason) == "" {
			return kernelProblem(CodeInvalidInput, "reason must be nonblank")
		}
	}
	if n.Kind == KindGroup && c.Op != OpBlock && c.Op != OpUnblock && c.Op != OpDrop {
		return kernelProblem(CodeInvalidTargetKind, "operation requires a task")
	}
	ids := []NodeID{n.ID}
	if n.Kind == KindGroup {
		ids = t.subtree(n.ID)
	}
	for _, id := range ids {
		leaf := t.state.Nodes[id]
		if leaf.Kind != KindTask {
			continue
		}
		old := leaf.Status
		switch c.Op {
		case OpStart:
			if old != Pending && old != InProgress {
				return kernelProblem(CodeInvalidTransition, "start requires a pending or active task; unblock/reopen first")
			}
			if old == InProgress {
				continue
			}
			if t.active != "" {
				previous := t.state.Nodes[t.active]
				previous.Status = Pending
				p.writes[previous.ID] = previous
			}
			leaf.Status = InProgress
		case OpDone:
			if old == Abandoned {
				return kernelProblem(CodeInvalidTransition, "abandoned task requires operator reopen before completion")
			}
			if old == Completed {
				continue
			}
			leaf.Status, leaf.Reason = Completed, ""
			if p.completed == nil {
				p.completed = make(map[NodeID]bool)
			}
			p.completed[id] = true
		case OpBlock:
			if old == Completed || old == Abandoned {
				if n.Kind == KindGroup {
					continue
				}
				return kernelProblem(CodeInvalidTransition, "terminal task cannot be blocked")
			}
			leaf.Status, leaf.Reason = Blocked, c.Reason
		case OpDrop:
			if old == Completed {
				if n.Kind == KindGroup {
					continue
				}
				return kernelProblem(CodeInvalidTransition, "completed task cannot be abandoned")
			}
			if old == Abandoned {
				continue
			}
			leaf.Status, leaf.Reason = Abandoned, c.Reason
		case OpUnblock:
			if old != Blocked {
				continue
			}
			leaf.Status, leaf.Reason = Pending, ""
		case OpReopen:
			if old != Completed && old != Abandoned {
				return kernelProblem(CodeInvalidTransition, "reopen requires a completed or abandoned task")
			}
			leaf.Status, leaf.Reason = Pending, ""
		}
		if leaf.Status != old || leaf.Reason != t.state.Nodes[id].Reason {
			p.writes[id] = leaf
			if old == InProgress && (c.Op == OpDone || c.Op == OpBlock || c.Op == OpDrop) {
				p.advance = true
			}
		}
	}
	return nil
}

func (t *Tree) planEdit(p *kernelPlan, actor Actor, n Node, c Command) error {
	if n.ID == RootID {
		return kernelProblem(CodeInvalidTargetKind, "root labels cannot be edited")
	}
	changed := false
	if c.Title != nil {
		if err := kernelLabel(*c.Title, t.limits.MaxTitleBytes); err != nil {
			return err
		}
		g := TitleGuard{Kind: n.Kind, Title: *c.Title}
		if t.guards[g] && actor.Kind == ActorAgent && *c.Title != n.Title {
			return kernelProblem(CodeRemovedByOperator, "operator-deleted label requires explicit operator restoration")
		}
		if actor.Kind == ActorOperator && t.guards[g] {
			if p.clearGuards == nil {
				p.clearGuards = make(map[TitleGuard]bool)
			}
			p.clearGuards[g] = true
		}
		changed = n.Title != *c.Title
		n.Title = *c.Title
	}
	if c.ActiveForm != nil {
		if n.Kind != KindTask {
			return kernelProblem(CodeInvalidTargetKind, "only tasks have active_form")
		}
		changed = changed || n.ActiveForm != *c.ActiveForm
		n.ActiveForm = *c.ActiveForm
	}
	if changed {
		p.writes[n.ID] = n
	}
	return nil
}

func kernelWithout(ids []NodeID, removed map[NodeID]bool) []NodeID {
	out := make([]NodeID, 0, len(ids))
	for _, id := range ids {
		if !removed[id] {
			out = append(out, id)
		}
	}
	return out
}

func kernelSameIDs(a, b []NodeID) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (t *Tree) planMove(p *kernelPlan, n Node, c Command) error {
	if n.ID == RootID {
		return kernelProblem(CodeInvalidTargetKind, "root cannot be moved")
	}
	destination, err := t.node(c.ParentID)
	if err != nil {
		return err
	}
	if destination.Kind != KindGroup {
		return kernelProblem(CodeInvalidTargetKind, "move destination must be a group")
	}
	if c.BeforeID == n.ID {
		return kernelProblem(CodeInvalidInput, "a node cannot be its own insertion anchor")
	}
	for id := destination.ID; id != ""; id = t.state.Nodes[id].ParentID {
		if id == n.ID {
			return kernelProblem(CodeInvalidInput, "move would create a cycle")
		}
	}
	if c.BeforeID != "" {
		anchor, err := t.node(c.BeforeID)
		if err != nil {
			return err
		}
		if anchor.ParentID != destination.ID {
			return kernelProblem(CodeInvalidInput, "before_id must be a direct child of the destination")
		}
	}
	type item struct {
		id    NodeID
		depth int
	}
	stack := []item{{n.ID, t.depth(destination.ID) + 1}}
	for len(stack) != 0 {
		x := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if x.depth > t.limits.MaxDepth {
			return kernelProblem(CodeLimitExceeded, "move exceeds depth limit")
		}
		for _, child := range t.state.Nodes[x.id].Children {
			stack = append(stack, item{child, x.depth + 1})
		}
	}
	oldParent := t.state.Nodes[n.ParentID]
	if destination.ID == oldParent.ID {
		destination.Children = kernelWithout(destination.Children, map[NodeID]bool{n.ID: true})
	} else {
		oldParent.Children = kernelWithout(oldParent.Children, map[NodeID]bool{n.ID: true})
		p.writes[oldParent.ID] = oldParent
	}
	children := make([]NodeID, 0, len(destination.Children)+1)
	for _, id := range destination.Children {
		if id == c.BeforeID {
			children = append(children, n.ID)
		}
		children = append(children, id)
	}
	if c.BeforeID == "" {
		children = append(children, n.ID)
	}
	if destination.ID == oldParent.ID && kernelSameIDs(children, oldParent.Children) {
		return nil
	}
	destination.Children = children
	p.writes[destination.ID] = destination
	if n.ParentID != destination.ID {
		n.ParentID = destination.ID
		p.writes[n.ID] = n
	}
	p.structure = true
	return nil
}

func (t *Tree) planRemove(p *kernelPlan, c Command) error {
	selected := make(map[NodeID]bool)
	if c.RemoveIDs != nil {
		for _, id := range c.RemoveIDs {
			if selected[id] {
				return kernelProblem(CodeInvalidInput, "duplicate removal IDs are forbidden")
			}
			if _, err := t.node(id); err != nil {
				return err
			}
			if id == RootID {
				return kernelProblem(CodeInvalidTargetKind, "root cannot be removed")
			}
			selected[id] = true
		}
	} else {
		n, err := t.selectNode(c.Target, false)
		if err != nil {
			return err
		}
		if n.ID == RootID {
			return kernelProblem(CodeInvalidTargetKind, "root cannot be removed")
		}
		selected[n.ID] = true
	}
	for _, id := range t.traversal() {
		n := t.state.Nodes[id]
		if selected[id] || p.removed[n.ParentID] {
			if p.removed == nil {
				p.removed = make(map[NodeID]bool)
			}
			p.removed[id] = true
			if id == t.active {
				p.advance = true
			}
			g := TitleGuard{Kind: n.Kind, Title: n.Title}
			if !t.guards[g] {
				if p.addGuards == nil {
					p.addGuards = make(map[TitleGuard]bool)
				}
				p.addGuards[g] = true
			}
		}
	}
	if len(p.removed) > t.limits.MaxTombstones-len(t.state.Tombstones) || len(p.addGuards) > t.limits.MaxTombstones-len(t.guards) {
		return kernelProblem(CodeLimitExceeded, "removal exceeds tombstone or title guard limit")
	}
	parents := make(map[NodeID]bool)
	for id := range p.removed {
		parent := t.state.Nodes[id].ParentID
		if !p.removed[parent] {
			parents[parent] = true
		}
	}
	for id := range parents {
		n := t.state.Nodes[id]
		n.Children = kernelWithout(n.Children, p.removed)
		p.writes[id] = n
	}
	p.structure = true
	return nil
}
