package jobbatch

// View returns a detached view of an unreleased node.
func (e *Engine) View(id ID) (NodeView, bool) {
	n, ok := e.nodes[id]
	if !ok {
		return NodeView{}, false
	}
	return n.view, true
}

// Snapshot returns an immutable pre-order snapshot of any unreleased batch.
func (e *Engine) Snapshot(id ID) (BatchView, bool) {
	n, ok := e.nodes[id]
	if !ok || n.task != nil {
		return BatchView{}, false
	}
	v := BatchView{Sequence: e.sequence, Node: n.view, Counts: e.counts(n)}
	var walk func(*treeNode)
	walk = func(p *treeNode) {
		for _, c := range p.children {
			v.Descendants = append(v.Descendants, c.view)
			walk(c)
		}
	}
	walk(n)
	return v, true
}

func (e *Engine) counts(n *treeNode) Counts {
	var counts Counts
	var walk func(*treeNode)
	walk = func(p *treeNode) {
		if p.task == nil {
			for _, c := range p.children {
				walk(c)
			}
			return
		}
		switch p.view.State {
		case StatePending:
			counts.Pending++
		case StateStarting, StateRunning, StateCancelling:
			counts.Active++
		case StateCompleted:
			counts.Completed++
		case StateFailed:
			counts.Failed++
		case StateCancelled:
			counts.Cancelled++
		case StateTimedOut:
			counts.TimedOut++
		case StateInterrupted:
			counts.Interrupted++
		case StateSkipped:
			counts.Skipped++
		}
	}
	walk(n)
	return counts
}
