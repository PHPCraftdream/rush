package tasktree

func kernelLeafProgress(status Status) Progress {
	p := Progress{Total: 1}
	switch status {
	case Pending:
		p.Pending = 1
	case InProgress:
		p.InProgress = 1
	case Completed:
		p.Completed = 1
	case Blocked:
		p.Blocked = 1
	case Abandoned:
		p.Abandoned = 1
	}
	return kernelFinishProgress(p)
}

func kernelFinishProgress(p Progress) Progress {
	p.Actionable = p.Pending + p.InProgress
	p.Unfinished = p.Actionable + p.Blocked
	p.Settled = p.Completed + p.Abandoned
	p.AllSettled = p.Total > 0 && p.Unfinished == 0
	p.AllCompleted = p.Total > 0 && p.Completed == p.Total
	return p
}

func kernelAddProgress(a, b Progress) Progress {
	a.Total += b.Total
	a.Pending += b.Pending
	a.InProgress += b.InProgress
	a.Completed += b.Completed
	a.Blocked += b.Blocked
	a.Abandoned += b.Abandoned
	return a
}

func (t *Tree) Summary() Summary {
	t.requireOwned()
	s := Summary{Initialized: t.state.Initialized, ActiveID: t.active}
	var pending NodeID
	for _, id := range t.traversal() {
		n := t.state.Nodes[id]
		if n.Kind == KindTask {
			s.Progress = kernelAddProgress(s.Progress, kernelLeafProgress(n.Status))
			if pending == "" && n.Status == Pending {
				pending = id
			}
		}
	}
	s.Progress = kernelFinishProgress(s.Progress)
	s.NextID = s.ActiveID
	if s.NextID == "" {
		s.NextID = pending
	}
	if s.ActiveID != "" {
		s.ActivePath = t.path(s.ActiveID)
	}
	if s.NextID != "" {
		s.NextPath = t.path(s.NextID)
	}
	return s
}

func (t *Tree) View(selector Selector) (View, error) {
	t.requireOwned()
	root, err := t.selectNode(selector, true)
	if err != nil {
		return View{}, err
	}
	ids := t.subtree(root.ID)
	views := make(map[NodeID]NodeView, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		n := t.state.Nodes[ids[i]]
		v := NodeView{ID: n.ID, ParentID: n.ParentID, Kind: n.Kind, Title: n.Title, ActiveForm: n.ActiveForm, Status: n.Status, Reason: n.Reason}
		if n.Kind == KindTask {
			v.Progress = kernelLeafProgress(n.Status)
		} else {
			if len(n.Children) > 0 {
				v.Children = make([]NodeView, 0, len(n.Children))
			}
			for _, child := range n.Children {
				cv := views[child]
				v.Children = append(v.Children, cv)
				v.Progress = kernelAddProgress(v.Progress, cv.Progress)
				delete(views, child)
			}
			v.Progress = kernelFinishProgress(v.Progress)
		}
		views[n.ID] = v
	}
	return View{Summary: t.Summary(), Root: views[root.ID]}, nil
}

func (t *Tree) Briefs(ids []NodeID) []NodeBrief {
	t.requireOwned()
	if ids == nil {
		return nil
	}
	out := make([]NodeBrief, 0, len(ids))
	for _, id := range ids {
		if n, ok := t.state.Nodes[id]; ok {
			out = append(out, NodeBrief{ID: n.ID, ParentID: n.ParentID, Kind: n.Kind, Title: n.Title, Status: n.Status, Reason: n.Reason})
		} else {
			_, removed := t.state.Tombstones[id]
			out = append(out, NodeBrief{ID: id, Removed: removed})
		}
	}
	return out
}
