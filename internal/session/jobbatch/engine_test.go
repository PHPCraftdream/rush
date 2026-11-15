package jobbatch

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func coreTestTask(group string) Spec { return Spec{Task: &TaskSpec{Kind: "controlled", Group: group}} }

func coreTestBatch(mode Mode, cap int, items ...Spec) Spec {
	return Spec{Batch: &BatchSpec{Mode: mode, Notify: NotifyEach, MaxParallel: cap, Items: items}}
}

func coreTestEngine(t *testing.T, cap int) *Engine {
	t.Helper()
	e, err := NewEngine(Limits{MaxConcurrent: cap, MaxDepth: 8, MaxNodes: 100})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func coreTestApply(t *testing.T, e *Engine, ev Event) Transition {
	t.Helper()
	tr, err := e.Apply(ev)
	if err != nil {
		t.Fatalf("event %+v: %v", ev, err)
	}
	coreTestInvariant(t, e, tr)
	return tr
}

func coreTestSubmit(t *testing.T, e *Engine, id ID, s Spec) Transition {
	t.Helper()
	return coreTestApply(t, e, Event{Type: EventSubmit, Root: id, Spec: s})
}

func coreTestFinish(t *testing.T, e *Engine, id ID, state State) Transition {
	t.Helper()
	n, _ := e.View(id)
	if !e.nodes[id].accepted {
		coreTestApply(t, e, Event{Type: EventStarted, Node: id, Token: n.Token})
	}
	return coreTestApply(t, e, Event{Type: EventSettled, Node: id, Token: n.Token, Result: Result{State: state}})
}

func coreTestReject(t *testing.T, e *Engine, ev Event, want error) {
	t.Helper()
	before := e.sequence
	views := map[ID]NodeView{}
	for id, n := range e.nodes {
		views[id] = n.view
	}
	tr, err := e.Apply(ev)
	if !errors.Is(err, want) || !reflect.DeepEqual(tr, Transition{}) || e.sequence != before {
		t.Fatalf("reject %+v: %+v %v", ev, tr, err)
	}
	for id, v := range views {
		if e.nodes[id].view != v {
			t.Fatal("rejection mutated", id)
		}
	}
}

func coreTestInvariant(t *testing.T, e *Engine, tr Transition) {
	t.Helper()
	active := 0
	groups := map[string]int{}
	for _, n := range e.nodes {
		if n.task != nil {
			if n.view.State.Active() {
				active++
				groups[n.task.Group]++
			}
			continue
		}
		live := false
		for _, c := range n.children {
			live = live || !c.view.State.Terminal()
		}
		if live == n.view.State.Terminal() {
			t.Fatalf("batch invariant %s %+v", n.view.ID, n.view)
		}
		if n.view.State.Terminal() {
			var check func(*treeNode)
			check = func(p *treeNode) {
				for _, c := range p.children {
					if !c.view.State.Terminal() {
						t.Fatalf("terminal batch %s has live descendant %s", n.view.ID, c.view.ID)
					}
					check(c)
				}
			}
			check(n)
		}
		if n.view.MaxParallel > 0 && e.subtreeActive(n) > n.view.MaxParallel {
			t.Fatal("subtree cap")
		}
	}
	if active != e.active || active > e.limits.MaxConcurrent {
		t.Fatal("global accounting")
	}
	for g := range e.groups {
		if e.groups[g] != groups[g] {
			t.Fatalf("group accounting %q: got %d want %d", g, e.groups[g], groups[g])
		}
	}
	for g, cap := range e.limits.ByGroup {
		if groups[g] > cap {
			t.Fatal("group cap", g)
		}
	}
	for g, a := range groups {
		if e.groups[g] != a {
			t.Fatal("group accounting")
		}
		if cap, ok := e.limits.ByGroup[g]; ok && a > cap {
			t.Fatal("group cap")
		}
	}
	for _, c := range tr.Commands {
		if c.Type == CommandStartLeaf {
			for n := e.nodes[c.Node]; n != nil; n = n.parent {
				if n.view.State.Terminal() || n.view.Cause != CauseNone {
					t.Fatal("start beneath terminal/cause", c)
				}
			}
		}
	}
	for _, notice := range tr.Notices {
		n := e.nodes[notice.Node]
		if !n.view.State.Terminal() {
			t.Fatal("nonterminal notice")
		}
		for _, other := range tr.Notices {
			if n.parent != nil && len(n.parent.children) == 1 && other.Node == n.parent.view.ID {
				t.Fatal("sole child notice")
			}
		}
	}
}

func TestCoreE1TaskRootRejected(t *testing.T) {
	e := coreTestEngine(t, 1)
	coreTestReject(t, e, Event{Type: EventSubmit, Root: "R", Spec: coreTestTask("")}, ErrInvalidSpec)
}

func TestCoreE2SequentialNestedBudgets(t *testing.T) {
	for _, cap := range []int{0, 1} {
		t.Run(string(rune('0'+cap)), func(t *testing.T) {
			e := coreTestEngine(t, 3)
			tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeSequential, cap, coreTestBatch(ModeParallel, 0, coreTestTask("g"), coreTestTask("h")), coreTestTask("h")))
			want := 2
			if cap == 1 {
				want = 1
			}
			if len(tr.Commands) != want {
				t.Fatalf("commands %v", tr.Commands)
			}
			coreTestFinish(t, e, "R.1.1", StateCompleted)
			coreTestFinish(t, e, "R.1.2", StateCompleted)
			v, _ := e.View("R.2")
			if v.State != StateStarting {
				t.Fatal(v)
			}
		})
	}
	e, err := NewEngine(Limits{MaxConcurrent: 4, ByGroup: map[string]int{"g": 1, "h": 2}, MaxDepth: 8, MaxNodes: 100})
	if err != nil {
		t.Fatal(err)
	}
	tr := coreTestSubmit(t, e, "M", coreTestBatch(ModeParallel, 0, coreTestTask("g"), coreTestTask("g"), coreTestTask("h"), coreTestTask("h")))
	if len(tr.Commands) != 3 {
		t.Fatal(tr.Commands)
	}
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "M.1"})
	if e.active != 3 {
		t.Fatal("stop freed unit")
	}
}

func TestCoreE3FailureSkippingPreservesTerminal(t *testing.T) {
	for _, state := range []State{StateFailed, StateTimedOut, StateInterrupted, StateCancelled} {
		e := coreTestEngine(t, 2)
		coreTestSubmit(t, e, "R", coreTestBatch(ModeSequential, 0, coreTestTask(""), coreTestBatch(ModeParallel, 0, coreTestTask("")), coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestBatch(ModeParallel, 0))))
		coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R.2"})
		coreTestFinish(t, e, "R.1", state)
		v, _ := e.View("R.2")
		if v.State != StateCancelled {
			t.Fatal(v)
		}
		v, _ = e.View("R.3")
		if state == StateCancelled {
			if v.State != StateRunning {
				t.Fatal(v)
			}
		} else if v.State != StateSkipped {
			t.Fatal(v)
		}
		empty, _ := e.View("R.3.2")
		if empty.State != StateCompleted {
			t.Fatal(empty)
		}
	}
	e := coreTestEngine(t, 1)
	s := coreTestBatch(ModeSequential, 0, coreTestTask(""), coreTestTask(""))
	s.Batch.OnFail = OnFailContinue
	coreTestSubmit(t, e, "C", s)
	tr := coreTestFinish(t, e, "C.1", StateFailed)
	if len(tr.Commands) != 1 || tr.Commands[0].Node != "C.2" {
		t.Fatal(tr)
	}
}

func TestCoreE4PersistentRoundRobin(t *testing.T) {
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask(""), coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestTask("")))
	tr := coreTestFinish(t, e, "A.1", StateCompleted)
	if tr.Commands[0].Node != "B.1" {
		t.Fatal(tr.Commands)
	}
	tr = coreTestFinish(t, e, "B.1", StateCompleted)
	if tr.Commands[0].Node != "A.2" {
		t.Fatal(tr.Commands)
	}
	tr = coreTestFinish(t, e, "A.2", StateCompleted)
	if tr.Commands[0].Node != "B.2" {
		t.Fatal(tr.Commands)
	}
}

func TestCoreE5CapacityGenerationBothOrdersStaleToken(t *testing.T) {
	for _, signalFirst := range []bool{false, true} {
		e := coreTestEngine(t, 1)
		tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("g")))
		old := tr.Commands[0].Token
		if signalFirst {
			coreTestApply(t, e, Event{Type: EventCapacityAvailable, Group: "g"})
		}
		tr = coreTestApply(t, e, Event{Type: EventStartRejected, Node: "R.1", Token: old, Reason: RejectCapacity})
		if !signalFirst {
			if len(tr.Commands) != 0 {
				t.Fatal("spin")
			}
			tr = coreTestApply(t, e, Event{Type: EventCapacityAvailable, Group: "g"})
		}
		if len(tr.Commands) != 1 || tr.Commands[0].Token <= old {
			t.Fatal(tr)
		}
		coreTestReject(t, e, Event{Type: EventStartRejected, Node: "R.1", Token: old, Reason: RejectCapacity}, ErrInvalidEvent)
		coreTestReject(t, e, Event{Type: EventDeadlineExpired, Node: "R.1", Token: old}, ErrInvalidEvent)
		coreTestFinish(t, e, "R.1", StateCompleted)
	}
}

func TestCoreE5SettledGenerationUnblocks(t *testing.T) {
	e := coreTestEngine(t, 2)
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("g"), coreTestTask("g")))
	n, _ := e.View("R.2")
	coreTestApply(t, e, Event{Type: EventStartRejected, Node: n.ID, Token: n.Token, Reason: RejectCapacity})
	tr := coreTestFinish(t, e, "R.1", StateCompleted)
	if e.generation["g"] != 1 || len(tr.Commands) != 1 || tr.Commands[0].Node != "R.2" {
		t.Fatal(tr, e.generation)
	}
}

func TestCoreE6ControlRetryTable(t *testing.T) {
	for _, target := range []ID{"R", "R.1"} {
		for _, siblingAcked := range []bool{false, true} {
			for _, code := range []string{"", CodeStopFailed, CodeStopPanic, CodeStopTimeout} {
				t.Run(string(target)+"/"+fmt.Sprint(siblingAcked)+"/"+code, func(t *testing.T) {
					e := coreTestEngine(t, 3)
					coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask(""), coreTestBatch(ModeParallel, 0, coreTestTask("")), coreTestTask("")))
					coreTestFinish(t, e, "R.3", StateCompleted)
					tr := coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R"})
					if len(tr.Commands) != 2 {
						t.Fatal(tr)
					}
					if siblingAcked {
						n, _ := e.View("R.2.1")
						coreTestApply(t, e, Event{Type: EventControlReported, Node: n.ID, Token: n.Token, Op: ControlOpStop})
					}
					for _, id := range []ID{target, "R.2", "R.2.1"} {
						tr = coreTestApply(t, e, Event{Type: EventStopRequested, Node: id})
						if len(tr.Commands) != 0 || len(tr.Changed) != 0 {
							t.Fatal("latched pending/acked retry changed nodes", id, tr)
						}
					}
					n, _ := e.View("R.1")
					coreTestApply(t, e, Event{Type: EventControlReported, Node: n.ID, Token: n.Token, Op: ControlOpStop, Code: code})
					coreTestReject(t, e, Event{Type: EventControlReported, Node: n.ID, Token: n.Token, Op: ControlOpStop}, ErrInvalidEvent)
					tr = coreTestApply(t, e, Event{Type: EventStopRequested, Node: target})
					want := 1
					if code == "" {
						want = 0
					}
					if len(tr.Commands) != want || len(tr.Changed) != want {
						t.Fatal(tr)
					}
					if code != "" {
						v, _ := e.View(n.ID)
						if tr.Changed[0] != v || !reflect.DeepEqual(tr.Commands[0], Command{Type: CommandStopLeaf, Node: n.ID, Token: n.Token, Cause: CauseStop}) || v.ControlError != code || !v.ControlPending {
							t.Fatal(v)
						}
					}
					tr = coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R"})
					if len(tr.Commands) != 0 || len(tr.Changed) != 0 {
						t.Fatal("fully latched nested subtree changed", tr)
					}
					coreTestFinish(t, e, "R.1", StateCompleted)
					tr = coreTestFinish(t, e, "R.2.1", StateCompleted)
					if tr.SettledRoots[0].State != StateCancelled || tr.SettledRoots[0].Counts.Completed != 1 {
						t.Fatal(tr)
					}
				})
			}
		}
	}
}

func TestCoreE6LastPendingStopSettlement(t *testing.T) {
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "A", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestSubmit(t, e, "B", coreTestBatch(ModeParallel, 0, coreTestBatch(ModeParallel, 0, coreTestTask("")), coreTestTask("")))
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "B.1"})
	v, _ := e.View("B")
	if v.State != StatePending {
		t.Fatal(v)
	}
	tr := coreTestApply(t, e, Event{Type: EventStopRequested, Node: "B.2"})
	if len(tr.Commands) != 0 || len(tr.SettledRoots) != 1 || tr.SettledRoots[0].Root != "B" || tr.SettledRoots[0].Counts.Cancelled != 2 {
		t.Fatal(tr)
	}
	v, _ = e.View("B")
	if v.Cause != CauseNone || v.State != StateCancelled {
		t.Fatal(v)
	}
	coreTestReject(t, e, Event{Type: EventAdd, Parent: "B", Items: []Spec{coreTestTask("")}}, ErrTerminal)
}

func TestCoreE7AddAtomicBoundsIDsEmpty(t *testing.T) {
	e, err := NewEngine(Limits{MaxConcurrent: 1, MaxDepth: 3, MaxNodes: 5})
	if err != nil {
		t.Fatal(err)
	}
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestReject(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestTask(""), {}}}, ErrInvalidSpec)
	coreTestReject(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestBatch(ModeParallel, 0, coreTestBatch(ModeParallel, 0, coreTestTask("")))}}, ErrInvalidSpec)
	tr := coreTestApply(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestBatch(ModeParallel, 0), coreTestTask("")}})
	if tr.Changed[0].ID != "R.2" || tr.Changed[1].ID != "R.3" {
		t.Fatal(tr.Changed)
	}
	coreTestApply(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestTask("")}})
	coreTestReject(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestTask("")}}, ErrInvalidSpec)
	coreTestReject(t, e, Event{Type: EventSubmit, Root: "R", Spec: coreTestBatch(ModeParallel, 0)}, ErrDuplicateRoot)
	z := coreTestEngine(t, 1)
	tr = coreTestSubmit(t, z, "Z", coreTestBatch(ModeParallel, 0, coreTestBatch(ModeSequential, 0), coreTestBatch(ModeParallel, 0, coreTestBatch(ModeParallel, 0))))
	if len(tr.Commands) != 0 || len(tr.SettledRoots) != 1 || tr.SettledRoots[0].Counts != (Counts{}) {
		t.Fatal(tr)
	}
	coreTestReject(t, z, Event{Type: EventAdd, Parent: "Z", Items: []Spec{coreTestTask("")}}, ErrTerminal)
}

func TestCoreFinalPendingDrainAndClosed(t *testing.T) {
	for _, mode := range []string{"none", "terminal", "pending", "starting", "running"} {
		t.Run(mode, func(t *testing.T) {
			e := coreTestEngine(t, 1)
			if mode == "terminal" {
				coreTestSubmit(t, e, "Z", coreTestBatch(ModeParallel, 0))
			}
			if mode == "pending" {
				tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
				coreTestApply(t, e, Event{Type: EventStartRejected, Node: "R.1", Token: tr.Commands[0].Token, Reason: RejectCapacity})
			}
			if mode == "starting" || mode == "running" {
				tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
				if mode == "running" {
					coreTestApply(t, e, Event{Type: EventStarted, Node: "R.1", Token: tr.Commands[0].Token})
				}
			}
			tr := coreTestApply(t, e, Event{Type: EventHostClosing})
			if mode == "starting" || mode == "running" {
				if tr.Final {
					t.Fatal("early final")
				}
				n, _ := e.View("R.1")
				coreTestReject(t, e, Event{Type: EventSubmit, Root: "X", Spec: coreTestBatch(ModeParallel, 0)}, ErrClosing)
				if mode == "starting" {
					tr = coreTestApply(t, e, Event{Type: EventStartRejected, Node: n.ID, Token: n.Token, Reason: RejectCancelled, Code: CodeStartFailed})
				} else {
					tr = coreTestFinish(t, e, n.ID, StateCompleted)
				}
			}
			if !tr.Final {
				t.Fatal("missing final")
			}
			for _, kind := range []EventType{EventSubmit, EventAdd, EventStopRequested, EventStarted, EventStartRejected, EventSettled, EventControlReported, EventCapacityAvailable, EventDeadlineExpired, EventHostClosing, EventRelease} {
				coreTestReject(t, e, Event{Type: kind}, ErrClosed)
			}
		})
	}
}
