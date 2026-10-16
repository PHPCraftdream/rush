package jobbatch

func (e *Engine) selectNotices(t *coreTransition) {
	var walk func(*treeNode)
	walk = func(n *treeNode) {
		for _, c := range n.children {
			walk(c)
		}
		if !t.settled[n.view.ID] {
			return
		}
		kind := n.view.Kind
		if n.parent == nil {
			kind = KindRoot
		} else {
			if n.parent.view.Notify != NotifyEach {
				return
			}
			for p := n.parent; p != nil; p = p.parent {
				if p.view.Notify == NotifyAll {
					return
				}
			}
			if len(n.parent.children) == 1 && t.settled[n.parent.view.ID] {
				return
			}
		}
		t.Notices = append(t.Notices, Notice{Root: n.view.Root, Node: n.view.ID, Kind: kind, State: n.view.State, Cause: n.view.Cause, Counts: e.counts(n)})
	}
	for _, root := range e.roots {
		if !root.released {
			walk(root)
		}
	}
}
