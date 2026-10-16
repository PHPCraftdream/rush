package jobbatch

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// V-b: public Add preserves groups/outcomes and rejected input has no record tail.
func TestRuntimeAddGroupsAndOutcomes(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 2, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask()))
	first := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	add := func(group, ref string, want ID) *runtimeTestAttempt {
		t.Helper()
		ids, err := r.Add(ctx, AddRequest{Parent: "R", Items: []Spec{{Task: &TaskSpec{Kind: "controlled", Group: group, Payload: json.RawMessage(`"PRIVATE_PAYLOAD"`)}}}, Ref: ref})
		if err != nil || !reflect.DeepEqual(ids, []ID{want}) {
			t.Fatal(ids, err)
		}
		a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
		if a.launch.ID != want || a.launch.Task.Group != group {
			t.Fatal(a.launch)
		}
		return a
	}
	second := add("g", "vb-g", "R.2")
	second.report(Result{State: StateCompleted, Code: "ok_a"})
	runtimeTestRecord(t, ctx, r, RecordSettled, "R.2")
	third := add("h", "vb-h", "R.3")
	runtimeTestFinish(t, ctx, r, first, StateCompleted)
	third.report(Result{State: StateFailed, Code: "exit_7"})
	runtimeTestRecord(t, ctx, r, RecordSettled, "R.3")
	if s := runtimeTestWait(t, ctx, b); s.State != StateFailed {
		t.Fatal(s)
	}
	for _, tc := range []struct {
		id          ID
		group, code string
		state       State
	}{{"R.2", "g", "ok_a", StateCompleted}, {"R.3", "h", "exit_7", StateFailed}} {
		v := runtimeTestView(t, ctx, r, tc.id)
		if v.Group != tc.group || v.Outcome.Code != tc.code || v.Outcome.State != tc.state || v.Attempts != 1 {
			t.Fatal(v)
		}
	}
	before := runtimeTestPage(t, ctx, r)
	if _, err := r.Add(ctx, AddRequest{Parent: "R", Items: []Spec{runtimeTestTask()}, Ref: "vb-late"}); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
	if after := runtimeTestPage(t, ctx, r); !reflect.DeepEqual(before, after) {
		t.Fatal("rejection appended a record")
	}
	runtimeTestClose(t, ctx, r)
	starts, settled := map[ID]int{}, map[ID]int{}
	for _, rec := range runtimeTestPage(t, ctx, r).Records {
		if rec.Ref == "vb-late" {
			t.Fatal(rec)
		}
		if rec.Type == RecordStarted {
			starts[rec.Node]++
		}
		if rec.Type == RecordSettled {
			settled[rec.Node]++
			if rec.Node == "R.2" && (rec.Group != "g" || rec.Code != "ok_a") {
				t.Fatal(rec)
			}
			if rec.Node == "R.3" && (rec.Group != "h" || rec.Code != "exit_7") {
				t.Fatal(rec)
			}
		}
	}
	for _, id := range []ID{"R.1", "R.2", "R.3"} {
		if starts[id] != 1 || settled[id] != 1 {
			t.Fatal(starts, settled)
		}
	}
}

// E9/R5: mutation after accepted handoff cannot change the detached Submit spec.
func TestRuntimeSubmitMutationAfterHandoff(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	hooks := &runnerRequestHooks{accepted: func(q *runnerRequest) {
		if q.event.Ref == "mutation" {
			close(entered)
			<-release
		}
	}}
	r, e, _, ctx := runtimeTestNew(t, 1, 128, hooks)
	task := &TaskSpec{Kind: "controlled", Group: "g", Payload: json.RawMessage(`"ORIGINAL"`)}
	spec := runtimeTestBatch(ModeSequential, NotifyAll, Spec{Task: task})
	type answer struct {
		b   *Batch
		err error
	}
	done := make(chan answer, 1)
	go func() {
		b, err := r.Submit(ctx, SubmitRequest{Root: "R", Spec: spec, Ref: "mutation"})
		done <- answer{b, err}
	}()
	runtimeTestReceive(t, ctx, entered)
	task.Payload[1] = 'X'
	task.Group = "changed"
	task.Kind = "changed"
	spec.Batch.Mode = ModeParallel
	spec.Batch.Items[0] = runtimeTestBatch(ModeParallel, NotifyAll)
	close(release)
	got := runtimeTestReceive(t, ctx, done)
	if got.err != nil {
		t.Fatal(got.err)
	}
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	if a.launch.Task.Kind != "controlled" || a.launch.Task.Group != "g" || string(a.launch.Task.Payload) != `"ORIGINAL"` {
		t.Fatal(a.launch)
	}
	s, err := r.Snapshot(ctx, "R")
	if err != nil || s.Node.Mode != ModeSequential || len(s.Descendants) != 1 {
		t.Fatal(s, err)
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, got.b)
	runtimeTestClose(t, ctx, r)
}

// E8: reported-state normalization and code privacy are independent of Start errors.
func TestRuntimeReportedOutcomesPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      State
		code, want string
		outcome    State
	}{
		{"invalid-state", StateRunning, "ok", CodeExecutorInvalidState, StateFailed},
		{"nonreserved", StateFailed, "exit_7", "exit_7", StateFailed},
		{"reserved", StateCompleted, CodeStopTimeout, CodeInvalid, StateCompleted},
		{"private", StateFailed, "PRIVATE_CODE/argv=secret", CodeInvalid, StateFailed},
		{"empty", StateCompleted, "", "", StateCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
			b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			a.report(Result{State: tc.state, Code: tc.code, Summary: "PRIVATE_SUMMARY", Details: json.RawMessage(`"PRIVATE_DETAILS"`)})
			runtimeTestWait(t, ctx, b)
			v := runtimeTestView(t, ctx, r, a.launch.ID)
			if v.Outcome.State != tc.outcome || v.Outcome.Code != tc.want || v.Outcome.Summary != "PRIVATE_SUMMARY" {
				t.Fatal(v)
			}
			runtimeTestClose(t, ctx, r)
			p := runtimeTestPage(t, ctx, r)
			raw, err := json.Marshal(p)
			if err != nil || strings.Contains(string(raw), "PRIVATE_") {
				t.Fatal("private result leaked into feed", err)
			}
			rec := runtimeTestRecord(t, ctx, r, RecordSettled, a.launch.ID)
			if rec.Code != tc.want || rec.State != tc.outcome {
				t.Fatal(rec)
			}
		})
	}
}

// F-e/R1: first Close deadline does not discard a subsequently accepted Start.
func TestRuntimeStartReturnsAfterCloseDeadline(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
	a := runtimeTestReceive(t, ctx, e.starts)
	if err := r.Close(runtimeTestCancelled()); !errors.Is(err, ErrCloseTimeout) {
		t.Fatal(err)
	}
	runtimeTestRecord(t, ctx, r, RecordHostClosing, "")
	runtimeTestReceive(t, ctx, a.ctx.Done())
	// §3.4 requires a detached context at dispatch. The worker cancels it
	// after actual return, so checking the escaped context is a false oracle.
	stops := make(chan error, 1)
	a.decision <- runtimeTestDecision{execution: &runtimeTestExecution{stop: func(c context.Context) error { stops <- c.Err(); return nil }}}
	if err := runtimeTestReceive(t, ctx, stops); err != nil {
		t.Fatal("Stop inherited expired waiter")
	}
	runtimeTestRecord(t, ctx, r, RecordStarted, a.launch.ID)
	runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
	if v := runtimeTestView(t, ctx, r, a.launch.ID); v.State != StateCancelling || v.Cause != CauseHostClosing {
		t.Fatal(v)
	}
	select {
	case <-b.Done():
		t.Fatal("Stop acknowledgement settled execution")
	default:
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	if s := runtimeTestWait(t, ctx, b); s.State != StateInterrupted {
		t.Fatal(s)
	}
	runtimeTestClose(t, ctx, r)
}

// C-b: a Stop panic emits a fixed code but cannot release the scheduling slot.
func TestRuntimeStopPanicHoldsSlot(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(context.Context) error { panic("PRIVATE_STOP") }})
	next := runtimeTestSubmit(t, ctx, r, "S", OneItem(TaskSpec{Kind: "controlled"}))
	if err := r.Stop(ctx, a.launch.ID); err != nil {
		t.Fatal(err)
	}
	if rec := runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID); rec.Code != CodeStopPanic {
		t.Fatal(rec)
	}
	if v := runtimeTestView(t, ctx, r, a.launch.ID); v.State != StateCancelling || v.ControlError != CodeStopPanic {
		t.Fatal(v)
	}
	if v := runtimeTestView(t, ctx, r, "S.1"); v.State != StatePending || v.Attempts != 0 {
		t.Fatal(v)
	}
	select {
	case <-e.starts:
		t.Fatal("panic released slot")
	default:
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	second := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, second, StateCompleted)
	runtimeTestWait(t, ctx, next)
	runtimeTestClose(t, ctx, r)
}

// R4/C-d: this execution intentionally implements no optional interfaces.
type runtimeTestBareExecution struct{}

func (*runtimeTestBareExecution) Stop(context.Context) error { return nil }

func TestRuntimeNoOptionalCapabilities(t *testing.T) {
	r, e, c, ctx := runtimeTestNew(t, 1, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestBareExecution{})
	if _, err := r.Output(ctx, a.launch.ID, 0); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if err := r.Inject(ctx, a.launch.ID, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
	// No item timeout or Stop exists: any timer here would be an illicit disposal.
	select {
	case <-c.armed:
		t.Fatal("optional disposal was attempted")
	default:
	}
	if err := r.Close(runtimeTestCancelled()); err != nil {
		t.Fatal(err)
	}
}
