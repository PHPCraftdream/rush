package jobbatch

import (
	"reflect"
	"testing"
)

func TestCoreAdmissionLatchedAncestorGuard(t *testing.T) {
	e := coreTestEngine(t, 1)
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	n := e.nodes["R.1"]
	coreTestApply(t, e, Event{Type: EventStartRejected, Node: n.view.ID, Token: n.view.Token, Reason: RejectCapacity})
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R"})
	// Probe the admission predicate against a pending proposal below the latched
	// ancestor. Public Apply also propagates the cause before admission.
	proposal := &treeNode{task: &TaskSpec{Kind: "controlled", Group: "unblocked"}, parent: e.roots[0], view: NodeView{State: StatePending}}
	if e.eligible(proposal) {
		t.Fatal("admission beneath terminal ancestor")
	}
	proposal.parent = &treeNode{view: NodeView{State: StateCancelling, Cause: CauseStop}}
	if e.eligible(proposal) {
		t.Fatal("admission beneath latched ancestor")
	}
}

func TestCoreStartLeafNeverUnderLatchedCause(t *testing.T) {
	e := coreTestEngine(t, 3)
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R"})
	tr := coreTestApply(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestBatch(ModeParallel, 0, coreTestTask("")), coreTestTask("")}})
	if len(tr.Commands) != 0 {
		t.Fatal("launch under stopped root", tr.Commands)
	}
	for _, id := range []ID{"R.2", "R.2.1", "R.3"} {
		v, _ := e.View(id)
		if v.State != StateCancelled || v.Cause != CauseStop || v.Attempts != 0 {
			t.Fatal(v)
		}
	}
	tr = coreTestFinish(t, e, "R.1", StateCompleted)
	if tr.SettledRoots[0].Counts.Cancelled != 3 {
		t.Fatal(tr)
	}
}

func TestCoreE8StaleDuplicateDeadlineAndCauseWinner(t *testing.T) {
	for _, first := range []EventType{EventStopRequested, EventDeadlineExpired} {
		e := coreTestEngine(t, 1)
		tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
		token := tr.Commands[0].Token
		ev := Event{Type: first, Node: "R.1"}
		if first == EventDeadlineExpired {
			ev.Token = token
		}
		coreTestApply(t, e, ev)
		if first == EventStopRequested {
			coreTestApply(t, e, Event{Type: EventDeadlineExpired, Node: "R.1", Token: token})
		} else {
			coreTestApply(t, e, Event{Type: EventStopRequested, Node: "R.1"})
		}
		coreTestApply(t, e, Event{Type: EventStarted, Node: "R.1", Token: token})
		coreTestReject(t, e, Event{Type: EventStarted, Node: "R.1", Token: token}, ErrInvalidEvent)
		tr = coreTestFinish(t, e, "R.1", StateCompleted)
		state := StateCancelled
		cause := CauseStop
		if first == EventDeadlineExpired {
			state = StateTimedOut
			cause = CauseTimeout
		}
		v, _ := e.View("R.1")
		if v.State != state || v.Cause != cause || v.Outcome.State != StateCompleted {
			t.Fatal(v)
		}
		if len(tr.SettledRoots) != 1 || e.active != 0 || e.generation[""] != 1 {
			t.Fatal(tr)
		}
		coreTestReject(t, e, Event{Type: EventSettled, Node: v.ID, Token: token, Result: Result{State: StateFailed}}, ErrTerminal)
		coreTestReject(t, e, Event{Type: EventControlReported, Node: v.ID, Token: token, Op: ControlOpStop}, ErrTerminal)
		coreTestReject(t, e, Event{Type: EventDeadlineExpired, Node: v.ID, Token: token}, ErrTerminal)
	}
}

func TestCoreEveryForbiddenEventField(t *testing.T) {
	samples := Event{Root: "R", Parent: "R", Node: "R.1", Spec: coreTestBatch(ModeParallel, 0), Items: []Spec{coreTestTask("")}, Ref: "ref", Token: 1, Reason: RejectFailure, Op: ControlOpStop, Code: CodeStartFailed, Result: Result{State: StateCompleted}, Group: "g"}
	allowed := map[EventType][]string{
		EventSubmit: {"Root", "Spec", "Ref"}, EventAdd: {"Parent", "Items", "Ref"}, EventStopRequested: {"Node"}, EventStarted: {"Node", "Token"}, EventStartRejected: {"Node", "Token", "Reason", "Code"}, EventSettled: {"Node", "Token", "Result"}, EventControlReported: {"Node", "Token", "Op", "Code"}, EventCapacityAvailable: {"Group"}, EventDeadlineExpired: {"Node", "Token"}, EventHostClosing: {}, EventRelease: {"Root"},
	}
	for kind, fields := range allowed {
		for i := 1; i < reflect.TypeOf(samples).NumField(); i++ {
			field := reflect.TypeOf(samples).Field(i).Name
			permitted := false
			for _, name := range fields {
				permitted = permitted || name == field
			}
			if permitted {
				continue
			}
			t.Run(string(kind)+"/"+field, func(t *testing.T) {
				e := coreTestEngine(t, 1)
				ev := Event{Type: kind}
				reflect.ValueOf(&ev).Elem().Field(i).Set(reflect.ValueOf(samples).Field(i))
				coreTestReject(t, e, ev, ErrInvalidEvent)
			})
		}
	}
}

func TestCoreAddLastSettleBothOrdersAndNestedDiscovery(t *testing.T) {
	e := coreTestEngine(t, 2)
	coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	coreTestApply(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestBatch(ModeParallel, 0, coreTestTask(""))}})
	coreTestFinish(t, e, "R.1", StateCompleted)
	tr := coreTestApply(t, e, Event{Type: EventAdd, Parent: "R.2", Items: []Spec{coreTestTask("g")}})
	if tr.Changed[0].ID != "R.2.2" {
		t.Fatal(tr)
	}
	snapshot, _ := e.Snapshot("R")
	var ids []ID
	for _, v := range snapshot.Descendants {
		ids = append(ids, v.ID)
	}
	if !reflect.DeepEqual(ids, []ID{"R.1", "R.2", "R.2.1", "R.2.2"}) {
		t.Fatal(ids)
	}
	coreTestFinish(t, e, "R.2.1", StateCompleted)
	coreTestFinish(t, e, "R.2.2", StateCompleted)
	coreTestReject(t, e, Event{Type: EventAdd, Parent: "R", Items: []Spec{coreTestTask("")}}, ErrTerminal)
}
