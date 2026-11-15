package jobbatch

func causeState(c Cause) State {
	switch c {
	case CauseStop:
		return StateCancelled
	case CauseTimeout:
		return StateTimedOut
	case CauseHostClosing:
		return StateInterrupted
	case CauseStopOnFail:
		return StateSkipped
	}
	return StateFailed
}

func (e *Engine) terminal(n *treeNode, s State, t *coreTransition) {
	n.view.State = s
	if n.task != nil {
		n.view.Control = ControlNone
		n.view.ControlPending = false
		n.view.ControlError = ""
	}
	t.settled[n.view.ID] = true
	t.change(n)
}

func (e *Engine) cancel(n *treeNode, c Cause, retry bool, t *coreTransition) {
	if n.view.State.Terminal() {
		return
	}
	fresh := n.view.Cause == CauseNone
	if fresh {
		n.view.Cause = c
		t.change(n)
	}
	if n.task == nil {
		if n.view.State == StateRunning {
			n.view.State = StateCancelling
			t.change(n)
		}
		for _, child := range n.children {
			e.cancel(child, c, retry, t)
		}
		return
	}
	if n.view.State == StatePending {
		e.terminal(n, causeState(n.view.Cause), t)
		return
	}
	if n.view.State != StateCancelling {
		n.view.State = StateCancelling
		t.change(n)
	}
	if fresh || retry && n.view.Control == ControlFailed {
		n.view.Control = ControlPending
		n.view.ControlPending = true
		t.change(n)
		t.Commands = append(t.Commands, Command{Type: CommandStopLeaf, Node: n.view.ID, Token: n.view.Token, Cause: n.view.Cause})
	}
}

func (e *Engine) settle(n *treeNode, t *coreTransition) {
	if n.task != nil || n.view.State.Terminal() {
		return
	}
	ready := true
	for i, c := range n.children {
		e.settle(c, t)
		if !c.view.State.Terminal() {
			ready = false
		}
		if c.view.State.Terminal() && n.view.Mode == ModeSequential && n.view.OnFail == OnFailStop && (c.view.State == StateFailed || c.view.State == StateTimedOut || c.view.State == StateInterrupted) {
			for _, successor := range n.children[i+1:] {
				e.cancel(successor, CauseStopOnFail, false, t)
				e.settle(successor, t)
			}
		}
	}
	if !ready {
		return
	}
	state := StateCompleted
	if n.view.Cause != CauseNone {
		state = causeState(n.view.Cause)
	} else {
		counts := e.counts(n)
		if counts.Failed+counts.TimedOut+counts.Interrupted > 0 {
			state = StateFailed
		} else if counts.Cancelled > 0 {
			state = StateCancelled
		}
	}
	e.terminal(n, state, t)
	if n.parent == nil {
		t.SettledRoots = append(t.SettledRoots, Summary{Root: n.view.ID, State: state, Counts: e.counts(n), Sequence: e.sequence})
	}
}
