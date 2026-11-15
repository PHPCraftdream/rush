package jobbatch

import "testing"

func coreTestClausesWithoutGlobal(e *Engine, n *treeNode) bool {
	if n.task == nil || n.view.State != StatePending || n.view.Cause != CauseNone || e.closing {
		return false
	}
	active := map[string]int{}
	for _, leaf := range e.nodes {
		if leaf.task != nil && leaf.view.State.Active() {
			active[leaf.view.Group]++
		}
	}
	if cap, ok := e.limits.ByGroup[n.view.Group]; ok && active[n.view.Group] >= cap {
		return false
	}
	if gen, ok := e.blocked[n.view.Group]; ok && gen == e.generation[n.view.Group] {
		return false
	}
	child := n
	for p := n.parent; p != nil; p = p.parent {
		if p.released || p.view.State.Terminal() || p.view.Cause != CauseNone {
			return false
		}
		if p.view.Mode == ModeSequential {
			for _, sibling := range p.children {
				if !sibling.view.State.Terminal() {
					if sibling != child {
						return false
					}
					break
				}
			}
		}
		if p.view.MaxParallel > 0 {
			count := 0
			var walk func(*treeNode)
			walk = func(x *treeNode) {
				if x.task != nil && x.view.State.Active() {
					count++
				}
				for _, c := range x.children {
					walk(c)
				}
			}
			walk(p)
			if count >= p.view.MaxParallel {
				return false
			}
		}
		child = p
	}
	return true
}

func TestCoreE5SettledBeforeLateCapacityRefusal(t *testing.T) {
	e := coreTestEngine(t, 2)
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("g"), coreTestTask("g")))
	old, _ := e.View("R.2")
	coreTestFinish(t, e, "R.1", StateCompleted)
	tr := coreTestApply(t, e, Event{Type: EventStartRejected, Node: old.ID, Token: old.Token, Reason: RejectCapacity})
	if len(tr.Commands) != 1 || tr.Commands[0].Node != old.ID || tr.Commands[0].Token <= old.Token {
		t.Fatal("lost settlement wake", tr)
	}
	retry := tr.Commands[0].Token
	tr = coreTestApply(t, e, Event{Type: EventStartRejected, Node: old.ID, Token: retry, Reason: RejectCapacity})
	if len(tr.Commands) != 0 {
		t.Fatal("no-signal refusal spun", tr)
	}
	v, _ := e.View(old.ID)
	if v.State != StatePending || v.Attempts != 2 || e.active != 0 || e.generation["g"] != 1 {
		t.Fatal(v)
	}
	tr = coreTestApply(t, e, Event{Type: EventCapacityAvailable, Group: "other"})
	if len(tr.Commands) != 0 {
		t.Fatal("unrelated signal unblocked group", tr)
	}
	coreTestReject(t, e, Event{Type: EventStartRejected, Node: old.ID, Token: old.Token, Reason: RejectCapacity}, ErrInvalidEvent)
}

func TestCoreE2NestedSequentialOrdering(t *testing.T) {
	e := coreTestEngine(t, 4)
	tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeSequential, 0, coreTestBatch(ModeSequential, 0, coreTestTask(""), coreTestTask("")), coreTestTask("")))
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "R.1.1" {
		t.Fatal(tr)
	}
	tr = coreTestFinish(t, e, "R.1.1", StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "R.1.2" {
		t.Fatal(tr)
	}
	v, _ := e.View("R.2")
	if v.State != StatePending {
		t.Fatal(v)
	}
	tr = coreTestFinish(t, e, "R.1.2", StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "R.2" {
		t.Fatal(tr)
	}
	nested, _ := e.View("R.1")
	if nested.State != StateCompleted {
		t.Fatal(nested)
	}
	coreTestFinish(t, e, "R.2", StateCompleted)
}

func TestCoreE2PositiveParallelAncestorCap(t *testing.T) {
	e := coreTestEngine(t, 5)
	tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 2, coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask(""), coreTestTask("")), coreTestTask("")))
	if len(tr.Commands) != 2 || e.active != 2 {
		t.Fatal(tr)
	}
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R.1.1"})
	v, _ := e.View("R.1.3")
	if v.State != StatePending {
		t.Fatal(v)
	}
	tr = coreTestFinish(t, e, "R.1.1", StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "R.1.3" || e.active != 2 {
		t.Fatal(tr)
	}
	tr = coreTestFinish(t, e, "R.1.2", StateCompleted)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "R.2" || e.active != 2 {
		t.Fatal(tr)
	}
}

func TestCoreE4NoGroupHeadOfLineBlockage(t *testing.T) {
	e := coreTestEngine(t, 2)
	tr := coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("g")))
	coreTestApply(t, e, Event{Type: EventStartRejected, Node: "A.1", Token: tr.Commands[0].Token, Reason: RejectCapacity})
	tr = coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask("g"), coreTestTask("h")))
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "B.2" {
		t.Fatal(tr)
	}
	for _, id := range []ID{"A.1", "B.1"} {
		v, _ := e.View(id)
		if v.State != StatePending {
			t.Fatal(v)
		}
	}
}

func TestCoreNoticeCancelledSoleDirectTask(t *testing.T) {
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	tr := coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R.1"})
	coreTestWantNotices(t, tr, "R")
	if tr.Notices[0].State != StateCancelled || tr.Notices[0].Cause != CauseNone || tr.Notices[0].Counts.Cancelled != 1 {
		t.Fatal(tr)
	}
}
