package jobbatch

import (
	"context"
	"errors"
	"testing"
	"time"
)

// W-a/W-b/E9: catches global/group accounting errors and ByGroup aliasing after NewRunner.
func TestRuntimeClosedAndOpenProfiles(t *testing.T) {
	for _, profile := range []string{"closed", "open", "unrankable"} {
		t.Run(profile, func(t *testing.T) {
			ctx := runtimeTestContext(t)
			groups := map[string]int{"agents": 1, "commands": 1}
			limit := 2
			if profile == "closed" {
				groups[""] = 1
				limit = 3
			}
			e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 32)}
			c := &runtimeTestClock{armed: make(chan *runtimeTestTimer, 128)}
			r, err := newRunnerWithClock(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: limit, ByGroup: groups, MaxDepth: 8, MaxNodes: 64}, Executor: e, ControlTimeout: 50 * time.Second, RecordRetention: 256}, c)
			if err != nil {
				t.Fatal(err)
			}
			groups["commands"] = 8
			aBatch := runtimeTestSubmit(t, ctx, r, "A", OneItem(TaskSpec{Kind: "controlled", Group: "agents"}))
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			group := ""
			if profile == "unrankable" {
				group = "commands"
			}
			cBatch := runtimeTestSubmit(t, ctx, r, "C", OneItem(TaskSpec{Kind: "controlled", Group: group}))
			waiter := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			// Messenger owns transport; the separate-root admission predicate is checked here.
			b := runtimeTestSubmit(t, ctx, r, "B", OneItem(TaskSpec{Kind: "controlled", Group: "commands"}))
			var command *runtimeTestAttempt
			if profile == "closed" {
				command = runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
				runtimeTestFinish(t, ctx, r, command, StateCompleted)
				runtimeTestWait(t, ctx, b)
				if err := r.Inject(ctx, waiter.launch.ID, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				if v := runtimeTestView(t, ctx, r, "B.1"); v.State != StatePending || v.Attempts != 0 {
					t.Fatal(v)
				}
				if err := r.Stop(ctx, waiter.launch.ID); err != nil {
					t.Fatal(err)
				}
				runtimeTestRecord(t, ctx, r, RecordControl, waiter.launch.ID)
				if v := runtimeTestView(t, ctx, r, "B.1"); v.State != StatePending {
					t.Fatal("ack freed slot", v)
				}
				runtimeTestFinish(t, ctx, r, waiter, StateCancelled)
				runtimeTestWait(t, ctx, cBatch)
				command = runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
				runtimeTestFinish(t, ctx, r, command, StateCompleted)
				runtimeTestWait(t, ctx, b)
			}
			runtimeTestFinish(t, ctx, r, a, StateCompleted)
			runtimeTestWait(t, ctx, aBatch)
			if profile == "closed" {
				runtimeTestFinish(t, ctx, r, waiter, StateCompleted)
				runtimeTestWait(t, ctx, cBatch)
			}
			runtimeTestClose(t, ctx, r)
		})
	}
}

// E9/P2-c: global headroom makes the group cap the only binding budget.
// The old profile oracle survives a Runtime cap=8 mutant; this case kills it.
func TestRuntimeGroupOnlyBindingOwnership(t *testing.T) {
	ctx := runtimeTestContext(t)
	groups := map[string]int{"agents": 1, "commands": 1}
	e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 32)}
	r, err := NewRunner(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: 16, ByGroup: groups, MaxDepth: 8, MaxNodes: 64}, Executor: e, ControlTimeout: time.Second, RecordRetention: 128})
	if err != nil {
		t.Fatal(err)
	}
	groups["commands"] = 8
	task := Spec{Task: &TaskSpec{Kind: "controlled", Group: "commands"}}
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyAll, task, task, task))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	for _, id := range []ID{"R.2", "R.3"} {
		if v := runtimeTestView(t, ctx, r, id); v.State != StatePending || v.Attempts != 0 {
			t.Fatalf("group-only cap breached with free global units: %s state=%s attempts=%d", id, v.State, v.Attempts)
		}
	}
	for range 2 {
		runtimeTestFinish(t, ctx, r, a, StateCompleted)
		a = runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
}

// W-c/W-d: catches hung Stop freeing group units and pending subtree Stop consuming or freeing units.
func TestRuntimeHungGroupAndPendingSubtree(t *testing.T) {
	ctx := runtimeTestContext(t)
	e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 32)}
	c := &runtimeTestClock{armed: make(chan *runtimeTestTimer, 128)}
	r, err := newRunnerWithClock(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: 3, ByGroup: map[string]int{"agents": 1, "commands": 1, "tools": 1}, MaxDepth: 8, MaxNodes: 64}, Executor: e, ControlTimeout: 50 * time.Second, RecordRetention: 256}, c)
	if err != nil {
		t.Fatal(err)
	}
	entered, unblock := make(chan struct{}, 1), make(chan struct{})
	xBatch := runtimeTestSubmit(t, ctx, r, "X", OneItem(TaskSpec{Kind: "controlled", Group: "commands"}))
	x := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { entered <- struct{}{}; <-unblock; return nil }})
	if err := r.Stop(ctx, x.launch.ID); err != nil {
		t.Fatal(err)
	}
	runtimeTestReceive(t, ctx, entered)
	runtimeTestReceive(t, ctx, c.armed)
	c.advance(50 * time.Second)
	runtimeTestRecord(t, ctx, r, RecordControl, x.launch.ID)
	aBatch := runtimeTestSubmit(t, ctx, r, "A", OneItem(TaskSpec{Kind: "controlled", Group: "agents"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	b := runtimeTestSubmit(t, ctx, r, "B", runtimeTestBatch(ModeParallel, NotifyAll, Spec{Task: &TaskSpec{Kind: "controlled", Group: "commands"}}, Spec{Task: &TaskSpec{Kind: "controlled", Group: "commands"}}))
	d := runtimeTestSubmit(t, ctx, r, "D", OneItem(TaskSpec{Kind: "controlled", Group: "agents"}))
	tools := runtimeTestSubmit(t, ctx, r, "T", OneItem(TaskSpec{Kind: "controlled", Group: "tools"}))
	tool := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, tool, StateCompleted)
	runtimeTestWait(t, ctx, tools)
	for _, id := range []ID{"B.1", "B.2", "D.1"} {
		if v := runtimeTestView(t, ctx, r, id); v.State != StatePending || v.Attempts != 0 {
			t.Fatal(v)
		}
	}
	if err := r.Stop(ctx, "B"); err != nil {
		t.Fatal(err)
	}
	if s := runtimeTestWait(t, ctx, b); s.Counts.Cancelled != 2 {
		t.Fatal(s)
	}
	if v := runtimeTestView(t, ctx, r, "D.1"); v.State != StatePending {
		t.Fatal(v)
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, aBatch)
	dependency := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, dependency, StateCompleted)
	runtimeTestWait(t, ctx, d)
	if err := r.Close(runtimeTestCancelled()); !errors.Is(err, ErrCloseTimeout) {
		t.Fatal(err)
	}
	runtimeTestRecord(t, ctx, r, RecordHostClosing, "")
	v := runtimeTestView(t, ctx, r, x.launch.ID)
	if v.Cause != CauseStop || v.State != StateCancelling {
		t.Fatal(v)
	}
	close(unblock)
	runtimeTestFinish(t, ctx, r, x, StateCancelled)
	runtimeTestWait(t, ctx, xBatch)
	runtimeTestClose(t, ctx, r)
}
