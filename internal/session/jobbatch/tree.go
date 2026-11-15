package jobbatch

import "fmt"

type treeNode struct {
	view               NodeView
	task               *TaskSpec
	parent             *treeNode
	children           []*treeNode
	depth              int
	authorized         uint64
	accepted, released bool
}

func (e *Engine) build(s Spec, id ID, parent *treeNode, ordinal, depth int, count *int) (*treeNode, error) {
	bad := func() (*treeNode, error) { return nil, fmt.Errorf("%w: subtree", ErrInvalidSpec) }
	*count++
	if depth > e.limits.MaxDepth || *count > e.limits.MaxNodes || (s.Task == nil) == (s.Batch == nil) {
		return bad()
	}
	n := &treeNode{parent: parent, depth: depth, view: NodeView{ID: id, Root: id, Ordinal: ordinal, State: StatePending}}
	if parent != nil {
		n.view.Parent = parent.view.ID
		n.view.Root = parent.view.Root
	}
	if s.Task != nil {
		if s.Task.Kind == "" || s.Task.Timeout < 0 {
			return bad()
		}
		task := *s.Task
		n.task = &task
		n.view.Kind = KindTask
		n.view.Group = task.Group
		n.view.Control = ControlNone
		return n, nil
	}
	b := *s.Batch
	if b.Mode != ModeParallel && b.Mode != ModeSequential {
		return bad()
	}
	if b.Notify == "" {
		b.Notify = NotifyAll
	}
	if b.Notify != NotifyAll && b.Notify != NotifyEach {
		return bad()
	}
	if b.MaxParallel < 0 {
		return bad()
	}
	if b.Mode == ModeParallel {
		if b.OnFail != "" {
			return nil, fmt.Errorf("%w: on_fail is only valid for sequential batches", ErrInvalidSpec)
		}
	} else {
		if b.OnFail == "" {
			b.OnFail = OnFailStop
		}
		if b.OnFail != OnFailStop && b.OnFail != OnFailContinue || b.MaxParallel > 1 {
			return bad()
		}
	}
	n.view.Kind = KindBatch
	n.view.Mode = b.Mode
	n.view.OnFail = b.OnFail
	n.view.Notify = b.Notify
	n.view.MaxParallel = b.MaxParallel
	for i, child := range b.Items {
		c, err := e.build(child, ID(fmt.Sprintf("%s.%d", id, i+1)), n, i+1, depth+1, count)
		if err != nil {
			return nil, err
		}
		n.children = append(n.children, c)
	}
	return n, nil
}

func (e *Engine) install(n *treeNode, t *coreTransition) {
	e.nodes[n.view.ID] = n
	t.created[n.view.ID] = true
	t.change(n)
	for _, c := range n.children {
		e.install(c, t)
	}
}

func (e *Engine) remove(n *treeNode) {
	delete(e.nodes, n.view.ID)
	for _, c := range n.children {
		e.remove(c)
	}
	n.task = nil
	n.children = nil
}

func (e *Engine) nodeCount(n *treeNode) int {
	total := 1
	for _, c := range n.children {
		total += e.nodeCount(c)
	}
	return total
}
