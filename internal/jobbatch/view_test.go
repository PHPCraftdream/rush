package jobbatch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCoreE9LimitsViewsAndFullLaunch(t *testing.T) {
	limits := Limits{MaxConcurrent: 3, ByGroup: map[string]int{"g": 1}, MaxDepth: 6, MaxNodes: 20}
	e, err := NewEngine(limits)
	if err != nil {
		t.Fatal(err)
	}
	limits.ByGroup["g"] = 3
	task := coreTestTask("g")
	task.Task.Payload = json.RawMessage(`{"sentinel":"payload"}`)
	task.Task.Timeout = time.Second
	tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestBatch(ModeParallel, 0, task, coreTestTask("g"))))
	if len(tr.Commands) != 1 {
		t.Fatal(tr)
	}
	launch := tr.Commands[0].Launch
	if launch.ID != "R.1.1" || launch.Token != tr.Commands[0].Token || launch.Task.Kind != "controlled" || launch.Task.Group != "g" || launch.Task.Timeout != time.Second || string(launch.Task.Payload) != string(task.Task.Payload) {
		t.Fatal(launch)
	}
	snapshot, ok := e.Snapshot("R.1")
	if !ok || len(snapshot.Descendants) != 2 || snapshot.Descendants[0].ID != "R.1.1" {
		t.Fatal(snapshot)
	}
	snapshot.Descendants[0].State = StateFailed
	v, _ := e.View("R.1.1")
	if v.State != StateStarting {
		t.Fatal(v)
	}
	data, err := json.Marshal(struct {
		Changed []NodeView
		Notices []Notice
	}{tr.Changed, tr.Notices})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "payload") {
		t.Fatal(string(data))
	}
	if _, ok = e.Snapshot("R.1.1"); ok {
		t.Fatal("task snapshot")
	}
}

func TestCoreValidationForbiddenFieldsAndCodes(t *testing.T) {
	e := coreTestEngine(t, 1)
	tr := coreTestSubmit(t, e, "R", coreTestBatch(ModeParallel, 0, coreTestTask("")))
	token := tr.Commands[0].Token
	for _, ev := range []Event{
		{Type: EventCapacityAvailable, Ref: "x"},
		{Type: EventHostClosing, Token: 1},
		{Type: EventStarted, Node: "R.1", Token: token, Group: "g"},
		{Type: EventStopRequested, Node: "R.1", Result: Result{Details: json.RawMessage{}}},
		{Type: EventStartRejected, Node: "R.1", Token: token, Reason: RejectCapacity, Code: "x"},
		{Type: EventStartRejected, Node: "R.1", Token: token, Reason: RejectFailure},
		{Type: EventStartRejected, Node: "R.1", Token: token, Reason: RejectFailure, Code: CodeStopFailed},
		{Type: EventStartRejected, Node: "R.1", Token: token, Reason: RejectFailure, Code: "PRIVATE/value"},
		{Type: EventSettled, Node: "R.1", Token: token, Result: Result{State: StateCompleted}},
	} {
		coreTestReject(t, e, ev, ErrInvalidEvent)
	}
	coreTestApply(t, e, Event{Type: EventStarted, Node: "R.1", Token: token})
	for _, result := range []Result{{State: StateSkipped}, {State: StateCompleted, Code: CodeStartFailed}, {State: StateFailed, Code: "BAD"}} {
		coreTestReject(t, e, Event{Type: EventSettled, Node: "R.1", Token: token, Result: result}, ErrInvalidEvent)
	}
	coreTestFinish(t, e, "R.1", StateCompleted)
	coreTestReject(t, e, Event{Type: EventSettled, Node: "R.1", Token: token, Result: Result{State: StateFailed}}, ErrTerminal)
	coreTestApply(t, e, Event{Type: EventRelease, Root: "R"})
	if _, ok := e.View("R.1"); ok {
		t.Fatal("released view")
	}
	coreTestReject(t, e, Event{Type: EventRelease, Root: "R"}, ErrReleased)
	coreTestReject(t, e, Event{Type: EventSubmit, Root: "R", Spec: coreTestBatch(ModeParallel, 0)}, ErrDuplicateRoot)
}

func TestCoreValidationSpecsAndLimits(t *testing.T) {
	for _, limits := range []Limits{{}, {MaxConcurrent: 1, MaxDepth: 1, MaxNodes: 1, ByGroup: map[string]int{"": 0}}} {
		if _, err := NewEngine(limits); err == nil {
			t.Fatal("limits accepted")
		}
	}
	e := coreTestEngine(t, 1)
	for _, s := range []Spec{{}, {Task: &TaskSpec{Kind: "x"}, Batch: &BatchSpec{Mode: ModeParallel}}, coreTestBatch("", 0), coreTestBatch(ModeSequential, 2), coreTestBatch(ModeParallel, -1)} {
		coreTestReject(t, e, Event{Type: EventSubmit, Root: "X", Spec: s}, ErrInvalidSpec)
	}
	for _, id := range []ID{"", "a.b"} {
		coreTestReject(t, e, Event{Type: EventSubmit, Root: id, Spec: coreTestBatch(ModeParallel, 0)}, ErrInvalidSpec)
	}
}
