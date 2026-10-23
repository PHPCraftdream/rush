package jobbatch

func (e *Engine) releaseUnit(n *treeNode) { e.active--; e.groups[n.task.Group]-- }

func (e *Engine) subtreeActive(n *treeNode) int {
	if n.task != nil {
		if n.view.State.Active() {
			return 1
		}
		return 0
	}
	total := 0
	for _, c := range n.children {
		total += e.subtreeActive(c)
	}
	return total
}

func (e *Engine) eligible(n *treeNode) bool {
	if n.task == nil || n.view.State != StatePending || n.view.Cause != CauseNone || e.closing || e.active >= e.limits.MaxConcurrent {
		return false
	}
	g := n.task.Group
	if cap, ok := e.limits.ByGroup[g]; ok && e.groups[g] >= cap {
		return false
	}
	if gen, ok := e.blocked[g]; ok && gen == e.generation[g] {
		return false
	}
	child := n
	for p := n.parent; p != nil; p = p.parent {
		if p.released || p.view.State.Terminal() || p.view.Cause != CauseNone {
			return false
		}
		if p.view.MaxParallel > 0 && e.subtreeActive(p) >= p.view.MaxParallel {
			return false
		}
		if p.view.Mode == ModeSequential {
			for _, c := range p.children {
				if !c.view.State.Terminal() {
					if c != child {
						return false
					}
					break
				}
			}
		}
		child = p
	}
	return true
}

func (e *Engine) candidate(n *treeNode) *treeNode {
	if n.released || n.view.State.Terminal() || n.view.Cause != CauseNone {
		return nil
	}
	if n.task != nil {
		if e.eligible(n) {
			return n
		}
		return nil
	}
	for _, c := range n.children {
		if leaf := e.candidate(c); leaf != nil {
			return leaf
		}
	}
	return nil
}

func (e *Engine) admit(t *coreTransition) {
	if e.closing || len(e.live) == 0 {
		return
	}
	visited := 0
	for e.active < e.limits.MaxConcurrent {
		var leaf *treeNode
		for scanned := 0; scanned < len(e.live); scanned++ {
			visited++
			index := (e.cursor + scanned) % len(e.live)
			if leaf = e.candidate(e.live[index]); leaf != nil {
				e.cursor = index + 1
				break
			}
		}
		if leaf == nil {
			break
		}
		e.token++
		leaf.view.Token = e.token
		leaf.view.Attempts++
		leaf.view.State = StateStarting
		leaf.accepted = false
		leaf.authorized = e.generation[leaf.task.Group]
		leaf.view.Control = ControlNone
		leaf.view.ControlPending = false
		leaf.view.ControlError = ""
		e.active++
		e.groups[leaf.task.Group]++
		t.change(leaf)
		for p := leaf.parent; p != nil; p = p.parent {
			if p.view.State == StatePending {
				p.view.State = StateRunning
				t.change(p)
			}
		}
		t.Commands = append(t.Commands, Command{Type: CommandStartLeaf, Node: leaf.view.ID, Token: leaf.view.Token, Launch: Launch{ID: leaf.view.ID, Token: leaf.view.Token, Task: *leaf.task}})
	}
	if e.walkInstrument != nil {
		e.walkInstrument("admit", visited)
	}
}
