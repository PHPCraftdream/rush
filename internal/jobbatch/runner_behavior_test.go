package jobbatch

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// R-b/R-c: catches missing bottom-up settlement of never-started nested batches and false launches of stopped successors.
func TestRuntimeSequentialPendingNestedSettlement(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 2, 128, nil)
	zero := runtimeTestSubmit(t, ctx, r, "Z", runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestBatch(ModeSequential, NotifyAll), runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestBatch(ModeParallel, NotifyAll))))
	if s := runtimeTestWait(t, ctx, zero); s.State != StateCompleted || s.Counts != (Counts{}) {
		t.Fatal(s)
	}
	if _, err := r.Add(ctx, AddRequest{Parent: "Z.2", Items: []Spec{runtimeTestTask()}}); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeSequential, NotifyEach, runtimeTestTask(), runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask(), runtimeTestTask()), runtimeTestTask()))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	if err := r.Stop(ctx, "R.2.1"); err != nil {
		t.Fatal(err)
	}
	if v := runtimeTestView(t, ctx, r, "R.2"); v.State != StatePending {
		t.Fatal(v)
	}
	if err := r.Stop(ctx, "R.2.2"); err != nil {
		t.Fatal(err)
	}
	if v := runtimeTestView(t, ctx, r, "R.2"); v.State != StateCancelled || v.Cause != CauseNone {
		t.Fatal(v)
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	last := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	if last.launch.ID != "R.3" {
		t.Fatal(last.launch)
	}
	runtimeTestFinish(t, ctx, r, last, StateCompleted)
	if s := runtimeTestWait(t, ctx, b); s.State != StateCancelled || s.Counts.Completed != 2 || s.Counts.Cancelled != 2 {
		t.Fatal(s)
	}
	runtimeTestClose(t, ctx, r)
	var notices []ID
	for _, rec := range runtimeTestPage(t, ctx, r).Records {
		if rec.Type == RecordNotice && rec.Root == "R" {
			notices = append(notices, rec.Node)
		}
	}
	if !reflect.DeepEqual(notices, []ID{"R.2", "R.1", "R.3", "R"}) {
		t.Fatal(notices)
	}
}

// C-c: catches batch retry roll-up incorrectly retrying acknowledged or terminal descendants.
func TestRuntimeSelectiveSubtreeStopRetry(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 3, 256, nil)
	calls := make(chan struct{}, 2)
	first := true
	x := &runtimeTestExecution{stop: func(context.Context) error {
		calls <- struct{}{}
		if first {
			first = false
			return errors.New("private")
		}
		return nil
	}}
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask(), runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask()), runtimeTestTask()))
	// Physical Start ordering is deliberately not assumed; bind executions by ID.
	attempts := make(map[ID]*runtimeTestAttempt)
	for range 3 {
		a := runtimeTestReceive(t, ctx, e.starts)
		attempts[a.launch.ID] = a
		execution := Execution(&runtimeTestExecution{})
		if a.launch.ID == "R.1" {
			execution = x
		}
		a.decision <- runtimeTestDecision{execution: execution}
		runtimeTestRecord(t, ctx, r, RecordStarted, a.launch.ID)
	}
	runtimeTestFinish(t, ctx, r, attempts["R.3"], StateCompleted)
	if err := r.Stop(ctx, "R"); err != nil {
		t.Fatal(err)
	}
	failed := runtimeTestRecord(t, ctx, r, RecordControl, "R.1")
	if failed.Code != CodeStopFailed {
		t.Fatal(failed)
	}
	runtimeTestRecord(t, ctx, r, RecordControl, "R.2.1")
	runtimeTestReceive(t, ctx, calls)
	if err := r.Stop(ctx, "R"); err != nil {
		t.Fatal(err)
	}
	runtimeTestReceive(t, ctx, calls)
	cursor := failed.Cursor + 1
	for {
		p, err := r.Events(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, rec := range p.Records {
			if rec.Type == RecordControl && rec.Node == "R.1" {
				if rec.Code != "" {
					t.Fatal(rec)
				}
				found = true
			}
		}
		if found {
			break
		}
		cursor = p.Next
	}
	if err := r.Stop(ctx, "R.2"); err != nil {
		t.Fatal(err)
	}
	runtimeTestFinish(t, ctx, r, attempts["R.1"], StateCancelled)
	runtimeTestFinish(t, ctx, r, attempts["R.2.1"], StateCancelled)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
	counts := map[ID]int{}
	for _, rec := range runtimeTestPage(t, ctx, r).Records {
		if rec.Type == RecordCommand && rec.Command == CommandStopLeaf {
			counts[rec.Node]++
		}
	}
	if counts["R.1"] != 2 || counts["R.2.1"] != 1 || counts["R.3"] != 0 {
		t.Fatal(counts)
	}
}

// R4: catches forwarding the wrong output cursor or rewriting executor-owned truncation/EOF semantics.
func TestRuntimeOutputCursorAndInjectPanic(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
	entered := make(chan uint64, 1)
	release := make(chan struct{})
	b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
	x := &runtimeTestExecution{read: func(_ context.Context, cursor uint64) (OutputChunk, error) {
		entered <- cursor
		<-release
		return OutputChunk{Data: []byte("B"), Next: 2, Oldest: 1, EOF: true, Truncated: true}, nil
	}, inject: func(context.Context, json.RawMessage) error { panic("private") }}
	a := runtimeTestAccept(t, ctx, r, e, x)
	if err := r.Inject(ctx, a.launch.ID, nil); !errors.Is(err, ErrExecutorPanic) {
		t.Fatal(err)
	}
	result := make(chan OutputChunk, 1)
	go func() {
		chunk, err := r.Output(ctx, a.launch.ID, 0)
		if err != nil {
			t.Error(err)
		}
		result <- chunk
	}()
	if cursor := runtimeTestReceive(t, ctx, entered); cursor != 0 {
		t.Fatal(cursor)
	}
	select {
	case <-result:
		t.Fatal("read returned before executor progress")
	default:
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	close(release)
	chunk := runtimeTestReceive(t, ctx, result)
	if string(chunk.Data) != "B" || chunk.Next != 2 || chunk.Oldest != 1 || !chunk.EOF || !chunk.Truncated {
		t.Fatal(chunk)
	}
	runtimeTestClose(t, ctx, r)
}
