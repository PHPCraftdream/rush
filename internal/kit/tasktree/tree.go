package tasktree

import (
	"strconv"
	"strings"
)

type Tree struct {
	state  Snapshot
	limits Limits
	active NodeID
	order  []NodeID
	guards map[TitleGuard]bool
	valid  bool
}

func kernelProblem(code ProblemCode, message string) error {
	return &Problem{Code: code, Message: message}
}

func kernelActor(actor Actor) error {
	if (actor.Kind != ActorAgent && actor.Kind != ActorOperator) || strings.TrimSpace(actor.ID) == "" {
		return kernelProblem(CodeInvalidInput, "actor requires agent/operator kind and a nonblank identity")
	}
	return nil
}

func kernelLabel(value string, max int) error {
	if strings.TrimSpace(value) == "" {
		return kernelProblem(CodeInvalidInput, "title must be nonblank")
	}
	if len(value) > max {
		return kernelProblem(CodeLimitExceeded, "title exceeds byte limit")
	}
	return nil
}

func (t *Tree) requireOwned() {
	if t == nil || !t.valid {
		panic("tasktree: Tree no longer owns state")
	}
}

func (t *Tree) traversal() []NodeID {
	if t.order == nil {
		t.order = t.subtree(RootID)
	}
	return t.order
}

func (t *Tree) subtree(root NodeID) []NodeID {
	ids := make([]NodeID, 0)
	stack := []NodeID{root}
	for len(stack) != 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		ids = append(ids, id)
		children := t.state.Nodes[id].Children
		for i := len(children) - 1; i >= 0; i-- {
			stack = append(stack, children[i])
		}
	}
	return ids
}

func (t *Tree) node(id NodeID) (Node, error) {
	if n, ok := t.state.Nodes[id]; ok {
		return n, nil
	}
	if _, ok := t.state.Tombstones[id]; ok {
		return Node{}, kernelProblem(CodeRemoved, "node was removed; use a current live ID")
	}
	return Node{}, kernelProblem(CodeNotFound, "node ID does not exist")
}

func kernelSelector(s Selector, allowEmpty bool) error {
	if s.ID != "" {
		if s.Text != "" || s.WithinID != "" {
			return kernelProblem(CodeInvalidInput, "ID cannot be combined with text or within_id")
		}
		return nil
	}
	if s.Text == "" && (!allowEmpty || s.WithinID != "") {
		return kernelProblem(CodeInvalidInput, "select exactly one ID or exact text")
	}
	return nil
}

func (t *Tree) selectNode(s Selector, allowEmpty bool) (Node, error) {
	if err := kernelSelector(s, allowEmpty); err != nil {
		return Node{}, err
	}
	if s.ID != "" {
		return t.node(s.ID)
	}
	if s.Text == "" {
		return t.state.Nodes[RootID], nil
	}
	scope := RootID
	if s.WithinID != "" {
		n, err := t.node(s.WithinID)
		if err != nil {
			return Node{}, err
		}
		if n.Kind != KindGroup {
			return Node{}, kernelProblem(CodeInvalidTargetKind, "within_id must name a group")
		}
		scope = n.ID
	}
	var matches []Node
	for _, id := range t.subtree(scope) {
		n := t.state.Nodes[id]
		if n.Title == s.Text {
			matches = append(matches, n)
		}
	}
	if len(matches) == 0 {
		return Node{}, kernelProblem(CodeNotFound, "no node has that exact title in the selected scope")
	}
	if len(matches) > 1 {
		p := &Problem{Code: CodeAmbiguousTarget, Message: "exact title matches multiple nodes; select a candidate ID"}
		for _, n := range matches {
			p.Candidates = append(p.Candidates, Candidate{ID: n.ID, Path: t.path(n.ID), Status: n.Status})
		}
		return Node{}, p
	}
	return matches[0], nil
}

func (t *Tree) path(id NodeID) []string {
	var out []string
	for id != "" {
		n := t.state.Nodes[id]
		out = append(out, n.Title)
		id = n.ParentID
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (t *Tree) depth(id NodeID) int {
	depth := 0
	for id != RootID {
		id = t.state.Nodes[id].ParentID
		depth++
	}
	return depth
}

func kernelCommand(c Command) error {
	target := c.Target != (Selector{})
	drafts := c.Drafts != nil
	ids := c.RemoveIDs != nil
	parent, before := c.ParentID != "", c.BeforeID != ""
	reason, title, form := c.Reason != "", c.Title != nil, c.ActiveForm != nil
	bad := false
	switch c.Op {
	case OpInit:
		bad = target || parent || before || reason || title || form || ids || len(c.Drafts) == 0
	case OpAdd:
		bad = target || before || reason || title || form || ids || len(c.Drafts) == 0
	case OpView:
		bad = drafts || parent || before || reason || title || form || ids
	case OpStart, OpDone, OpUnblock, OpReopen:
		bad = !target || drafts || parent || before || reason || title || form || ids
	case OpBlock, OpDrop:
		bad = !target || drafts || parent || before || title || form || ids || strings.TrimSpace(c.Reason) == ""
	case OpEdit:
		bad = !target || drafts || parent || before || reason || ids || (!title && !form)
	case OpMove:
		bad = !target || drafts || !parent || reason || title || form || ids
	case OpRemove:
		bad = drafts || parent || before || reason || title || form || (target == ids) || (ids && len(c.RemoveIDs) == 0)
	default:
		bad = true
	}
	if bad {
		return kernelProblem(CodeInvalidInput, "invalid operation or irrelevant/conflicting operation fields")
	}
	if target {
		return kernelSelector(c.Target, c.Op == OpView)
	}
	return nil
}

type kernelPlan struct {
	writes      map[NodeID]Node
	created     map[NodeID]bool
	removed     map[NodeID]bool
	completed   map[NodeID]bool
	clearGuards map[TitleGuard]bool
	addGuards   map[TitleGuard]bool
	next        uint64
	initialize  bool
	structure   bool
	advance     bool
}

func newKernelPlan(next uint64) *kernelPlan {
	return &kernelPlan{writes: make(map[NodeID]Node), next: next}
}

func (p *kernelPlan) get(t *Tree, id NodeID) Node {
	if n, ok := p.writes[id]; ok {
		return n
	}
	return t.state.Nodes[id]
}

func (t *Tree) Apply(actor Actor, command Command) (Delta, error) {
	t.requireOwned()
	if err := kernelActor(actor); err != nil {
		return Delta{}, err
	}
	if err := kernelCommand(command); err != nil {
		return Delta{}, err
	}
	if (command.Op == OpRemove || command.Op == OpReopen) && actor.Kind != ActorOperator {
		return Delta{}, kernelProblem(CodeForbidden, "only operators may remove or reopen nodes")
	}
	if command.Op == OpView {
		_, err := t.selectNode(command.Target, true)
		return Delta{}, err
	}
	if command.Op == OpInit {
		if t.state.Initialized {
			return Delta{}, kernelProblem(CodeAlreadyInitialized, "init cannot replace an initialized board")
		}
	} else if !t.state.Initialized {
		return Delta{}, kernelProblem(CodeUninitialized, "initialize the board before mutating it")
	}
	p := newKernelPlan(t.state.NextID)
	var err error
	switch command.Op {
	case OpInit, OpAdd:
		err = t.planAdd(p, actor, command)
	case OpRemove:
		err = t.planRemove(p, command)
	default:
		var n Node
		n, err = t.selectNode(command.Target, false)
		if err == nil {
			switch command.Op {
			case OpEdit:
				err = t.planEdit(p, actor, n, command)
			case OpMove:
				err = t.planMove(p, n, command)
			default:
				err = t.planStatus(p, n, command)
			}
		}
	}
	if err != nil {
		return Delta{}, err
	}
	return t.applyPlan(p, actor), nil
}

func (t *Tree) applyPlan(p *kernelPlan, actor Actor) Delta {
	var oldRemoved []NodeID
	if len(p.removed) != 0 {
		for _, id := range t.traversal() {
			if p.removed[id] {
				oldRemoved = append(oldRemoved, id)
			}
		}
	}
	for id, n := range p.writes {
		t.state.Nodes[id] = n
		if id == t.active && n.Status != InProgress {
			t.active = ""
		}
	}
	for _, id := range oldRemoved {
		n := t.state.Nodes[id]
		if t.state.Tombstones == nil {
			t.state.Tombstones = make(map[NodeID]Tombstone)
		}
		t.state.Tombstones[id] = Tombstone{ID: id, Kind: n.Kind, Title: n.Title, Actor: actor}
		delete(t.state.Nodes, id)
		if t.active == id {
			t.active = ""
		}
	}
	for id, n := range p.writes {
		if n.Status == InProgress && !p.removed[id] {
			t.active = id
		}
	}
	if p.structure {
		t.order = nil
	}
	if p.initialize {
		t.state.Initialized = true
	}
	t.state.NextID = p.next
	if len(p.clearGuards) != 0 {
		kept := t.state.TitleGuards[:0]
		for _, g := range t.state.TitleGuards {
			if p.clearGuards[g] {
				delete(t.guards, g)
			} else {
				kept = append(kept, g)
			}
		}
		t.state.TitleGuards = kept
	}
	for _, id := range oldRemoved {
		n := t.state.Tombstones[id]
		g := TitleGuard{Kind: n.Kind, Title: n.Title}
		if p.addGuards[g] && !t.guards[g] {
			t.guards[g] = true
			t.state.TitleGuards = append(t.state.TitleGuards, g)
		}
	}
	if p.advance && t.active == "" {
		for _, id := range t.traversal() {
			n := t.state.Nodes[id]
			if n.Status == Pending {
				n.Status = InProgress
				t.state.Nodes[id] = n
				p.writes[id] = n
				t.active = id
				break
			}
		}
	}
	delta := Delta{Removed: oldRemoved}
	if len(p.writes) != 0 {
		for _, id := range t.traversal() {
			if p.created[id] {
				delta.Created = append(delta.Created, id)
			} else if _, ok := p.writes[id]; ok {
				delta.Updated = append(delta.Updated, id)
				if p.completed[id] {
					delta.Completed = append(delta.Completed, id)
				}
			}
		}
	}
	return delta
}

func kernelID(counter uint64) NodeID { return NodeID("n" + strconv.FormatUint(counter, 36)) }
