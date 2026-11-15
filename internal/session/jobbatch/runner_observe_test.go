package jobbatch

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// F-a/F-b/F-d/R2/R3: catches Final-after-Done, reader-dependent Close and post-F mutations.
func TestRuntimeFinalFeedAndFrozenReads(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 2, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyEach, runtimeTestTask(), runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask())))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	other := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	cut := runtimeTestPage(t, ctx, r).Head
	done := make(chan error, 1)
	go func() { done <- r.Close(ctx) }()
	runtimeTestRecord(t, ctx, r, RecordHostClosing, "")
	runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
	runtimeTestRecord(t, ctx, r, RecordControl, other.launch.ID)
	runtimeTestFinish(t, ctx, r, a, StateInterrupted)
	runtimeTestFinish(t, ctx, r, other, StateInterrupted)
	summary := runtimeTestWait(t, ctx, b)
	// Done is authoritative: the closed record must already be readable, before Close joins.
	p := runtimeTestPage(t, ctx, r)
	if p.Final == 0 || p.Head != p.Final || p.Records[len(p.Records)-1].Type != RecordClosed {
		t.Fatal(p)
	}
	if p.Records[len(p.Records)-1].Sequence != summary.Sequence {
		t.Fatal("Final sequence differs from final-root settlement")
	}
	if err := runtimeTestReceive(t, ctx, done); err != nil {
		t.Fatal(err)
	}
	tail, err := r.Events(runtimeTestCancelled(), cut+1)
	if err != nil || len(tail.Records) == 0 || tail.Next != p.Final+1 {
		t.Fatal(tail, err)
	}
	for i, rec := range p.Records {
		if rec.Cursor != uint64(i+1) {
			t.Fatal("cursor hole", rec)
		}
	}
	results := make(chan EventPage, 2)
	for range 2 {
		go func() {
			page, err := r.Events(runtimeTestCancelled(), 1)
			if err != nil {
				t.Error(err)
			}
			results <- page
		}()
	}
	for range 2 {
		if got := runtimeTestReceive(t, ctx, results); !reflect.DeepEqual(got, p) {
			t.Fatal("consuming feed")
		}
	}
	for _, cursor := range []uint64{p.Final + 1, p.Final + 3} {
		eof, err := r.Events(runtimeTestCancelled(), cursor)
		if !errors.Is(err, ErrClosed) || eof.Next != p.Final+1 || len(eof.Records) != 0 {
			t.Fatal(eof, err)
		}
	}
	snapshot, err := r.Snapshot(ctx, "R")
	if err != nil || snapshot.Sequence != summary.Sequence || snapshot.Counts.Interrupted != 2 {
		t.Fatal(snapshot, err)
	}
	snapshot.Descendants[0].State = StateFailed
	if v := runtimeTestView(t, ctx, r, snapshot.Descendants[0].ID); v.State != StateInterrupted {
		t.Fatal(v)
	}
	mutations := []func() error{
		func() error { return r.Stop(ctx, "R") }, func() error { return r.Release(ctx, "R") }, func() error { return r.CapacityAvailable(ctx, "") },
		func() error { _, err := r.Output(ctx, a.launch.ID, 0); return err }, func() error { return r.Inject(ctx, a.launch.ID, nil) },
		func() error {
			_, err := r.Submit(ctx, SubmitRequest{Root: "X", Spec: OneItem(TaskSpec{Kind: "controlled"})})
			return err
		},
		func() error {
			_, err := r.Add(ctx, AddRequest{Parent: "R", Items: []Spec{runtimeTestTask()}})
			return err
		},
	}
	for _, mutation := range mutations {
		if err := mutation(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	}
	a.report(Result{State: StateFailed})
	other.report(Result{State: StateCompleted})
	if got := runtimeTestPage(t, ctx, r); !reflect.DeepEqual(got, p) {
		t.Fatal("late report changed feed")
	}
	runtimeTestClose(t, ctx, r)
}

// F-a/R2/P2-d: observe Done directly while publication is held, not after
// a Settled-record await. Moving appendTransition after Done survives the old
// record-first oracle but fails this assertion (including a missing Done).
func TestRuntimeDonePublishesWholeFinalTransition(t *testing.T) {
	type publication struct {
		batch      *Batch
		transition Transition
		resume     chan struct{}
	}
	published := make(chan publication)
	hooks := &runnerRequestHooks{donePublished: func(b *Batch, tr Transition) {
		resume := make(chan struct{})
		published <- publication{b, tr, resume}
		<-resume
	}}
	r, e, _, ctx := runtimeTestNew(t, 2, 128, hooks)
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyEach, runtimeTestTask(), runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask())))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	other := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	if a.launch.ID != "R.1" {
		a, other = other, a
	}
	closing := make(chan error, 1)
	go func() { closing <- r.Close(ctx) }()
	runtimeTestRecord(t, ctx, r, RecordHostClosing, "")
	runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
	runtimeTestRecord(t, ctx, r, RecordControl, other.launch.ID)
	runtimeTestFinish(t, ctx, r, a, StateInterrupted)
	checked := make(chan string, 1)
	go func() {
		pub := <-published
		defer close(pub.resume) // assertion failures must unblock the loop too
		select {
		case <-pub.batch.Done():
		default:
			checked <- "Done not closed at publication seam"
			return
		}
		// A cancelled context prevents a missing record from turning into a hang.
		p, err := r.Events(runtimeTestCancelled(), 1)
		if err != nil {
			checked <- "feed unavailable at Done"
			return
		}
		var tail []Record
		for _, rec := range p.Records {
			if rec.Sequence == pub.transition.Sequence {
				tail = append(tail, rec)
			}
		}
		wantTypes := []RecordType{RecordSettled, RecordNotice, RecordNotice, RecordClosed}
		wantNodes := []ID{"R.2.1", "R.2", "R", ""}
		if pub.batch != b || !pub.transition.Final || len(tail) != len(wantTypes) {
			checked <- "Done preceded ALL final transition records"
			return
		}
		for i, rec := range tail {
			if rec.Type != wantTypes[i] || rec.Node != wantNodes[i] || rec.Cursor != tail[0].Cursor+uint64(i) {
				checked <- "incorrect final transition records at Done"
				return
			}
		}
		if p.Final == 0 || p.Head != p.Final || tail[3].Cursor != p.Final || b.summary.Sequence != tail[3].Sequence {
			checked <- "closed/Final missing at Done"
			return
		}
		checked <- ""
	}()
	other.report(Result{State: StateInterrupted})
	if failure := runtimeTestReceive(t, ctx, checked); failure != "" {
		t.Error(failure)
	}
	runtimeTestWait(t, ctx, b)
	if err := runtimeTestReceive(t, ctx, closing); err != nil {
		t.Fatal(err)
	}
}

// F-c/F-d: catches silent retention gaps, mutable frozen windows and wrong stateless-tail sequence.
func TestRuntimeFrozenRetentionAndTail(t *testing.T) {
	for _, retention := range []int{1, 4, 128} {
		t.Run(string(rune('a'+retention)), func(t *testing.T) {
			r, _, _, ctx := runtimeTestNew(t, 1, retention, nil)
			b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyAll))
			summary := runtimeTestWait(t, ctx, b)
			if err := r.CapacityAvailable(ctx, "g"); err != nil {
				t.Fatal(err)
			}
			runtimeTestClose(t, ctx, r)
			p, err := r.Events(runtimeTestCancelled(), 1)
			if retention < 128 {
				if !errors.Is(err, ErrGap) || p.Next != p.Oldest || len(p.Records) != 0 {
					t.Fatal(p, err)
				}
				p, err = r.Events(runtimeTestCancelled(), p.Oldest)
			}
			if err != nil || p.Final != p.Head || p.Records[len(p.Records)-1].Type != RecordClosed {
				t.Fatal(p, err)
			}
			if last := p.Records[len(p.Records)-1]; last.Sequence != summary.Sequence+2 {
				t.Fatal(last, summary)
			}
			repeat, err := r.Events(runtimeTestCancelled(), p.Oldest)
			if err != nil || !reflect.DeepEqual(repeat, p) {
				t.Fatal(repeat, err)
			}
			snapshot, err := r.Snapshot(ctx, "R")
			if err != nil || snapshot.Sequence != summary.Sequence+2 {
				t.Fatal(snapshot, err)
			}
			runtimeTestClose(t, ctx, r)
		})
	}
	// No-root Close must publish HostClosing and exactly one closed record.
	r, _, _, ctx := runtimeTestNew(t, 1, 128, nil)
	runtimeTestClose(t, ctx, r)
	p := runtimeTestPage(t, ctx, r)
	if len(p.Records) != 2 || p.Records[0].Type != RecordHostClosing || p.Records[1].Type != RecordClosed {
		t.Fatal(p)
	}
}

// E1/E9/R5/V-b: catches aliased ingress payloads, wrapped nested Add leaves, duplicate IDs and mutable snapshots/records.
func TestRuntimeNestedAddOwnershipAndDiscovery(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 1, 256, nil)
	raw := TaskSpec{Kind: "original", Group: "g", Payload: json.RawMessage(`{"secret":"PAYLOAD_SENTINEL"}`)}
	helper := OneItem(raw)
	raw.Payload[2] = 'X'
	if strings.Contains(string(helper.Batch.Items[0].Task.Payload), "X") {
		t.Fatal("OneItem payload aliases input")
	}
	if _, err := r.Submit(ctx, SubmitRequest{Root: "bad", Spec: runtimeTestTask()}); !errors.Is(err, ErrInvalidSpec) {
		t.Fatal(err)
	}
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyEach, runtimeTestBatch(ModeParallel, NotifyEach, helper.Batch.Items[0])))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	payload := json.RawMessage(`{"value":"ADD_SENTINEL"}`)
	nested := runtimeTestBatch(ModeParallel, NotifyEach, Spec{Task: &TaskSpec{Kind: "deep", Group: "h", Timeout: time.Hour, Payload: payload}})
	ids, err := r.Add(ctx, AddRequest{Parent: "R.1", Items: []Spec{runtimeTestBatch(ModeParallel, NotifyAll), nested}, Ref: "add"})
	if err != nil || !reflect.DeepEqual(ids, []ID{"R.1.2", "R.1.3"}) {
		t.Fatal(ids, err)
	}
	payload[2] = 'X'
	nested.Batch.Items[0].Task.Kind = "changed"
	snapshot, err := r.Snapshot(ctx, "R")
	if err != nil {
		t.Fatal(err)
	}
	want := []ID{"R.1", "R.1.1", "R.1.2", "R.1.3", "R.1.3.1"}
	var discovered []ID
	for _, v := range snapshot.Descendants {
		discovered = append(discovered, v.ID)
	}
	if !reflect.DeepEqual(discovered, want) {
		t.Fatal(discovered)
	}
	add := runtimeTestRecord(t, ctx, r, RecordAdd, "R.1")
	if !reflect.DeepEqual(add.IDs, []ID{"R.1.2", "R.1.3", "R.1.3.1"}) {
		t.Fatal(add.IDs)
	}
	add.IDs[0] = "corrupt"
	again := runtimeTestRecord(t, ctx, r, RecordAdd, "R.1")
	if again.IDs[0] != "R.1.2" {
		t.Fatal("record ID alias")
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	deep := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	if deep.launch.ID != "R.1.3.1" || deep.launch.Task.Kind != "deep" || deep.launch.Task.Group != "h" || deep.launch.Task.Timeout != time.Hour || string(deep.launch.Task.Payload) != `{"value":"ADD_SENTINEL"}` {
		t.Fatal(deep.launch)
	}
	deep.report(Result{State: StateFailed, Code: "exit_7", Summary: "SUMMARY_SENTINEL", Details: json.RawMessage(`"DETAIL_SENTINEL"`)})
	runtimeTestRecord(t, ctx, r, RecordSettled, deep.launch.ID)
	summary := runtimeTestWait(t, ctx, b)
	if summary.State != StateFailed {
		t.Fatal(summary)
	}
	runtimeTestClose(t, ctx, r)
	bytes, err := json.Marshal(runtimeTestPage(t, ctx, r))
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{"PAYLOAD_SENTINEL", "ADD_SENTINEL", "SUMMARY_SENTINEL", "DETAIL_SENTINEL"} {
		if strings.Contains(string(bytes), sentinel) {
			t.Fatal("feed leaks", sentinel)
		}
	}
}

// R4/E9: catches injected message aliasing, unsupported/not-started confusion and capability panic escaping.
func TestRuntimeCapabilities(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask())))
	a := runtimeTestReceive(t, ctx, e.starts)
	if _, err := r.Output(ctx, a.launch.ID, 0); !errors.Is(err, ErrNotStarted) {
		t.Fatal(err)
	}
	if err := r.Inject(ctx, "R", nil); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	received := make(chan json.RawMessage, 1)
	x := &runtimeTestExecution{inject: func(_ context.Context, m json.RawMessage) error { received <- m; return nil }, read: func(context.Context, uint64) (OutputChunk, error) { panic("private") }}
	a.decision <- runtimeTestDecision{execution: x}
	runtimeTestRecord(t, ctx, r, RecordStarted, a.launch.ID)
	msg := json.RawMessage(`"MESSAGE_SENTINEL"`)
	if err := r.Inject(ctx, a.launch.ID, msg); err != nil {
		t.Fatal(err)
	}
	copy := runtimeTestReceive(t, ctx, received)
	msg[1] = 'X'
	if string(copy) != `"MESSAGE_SENTINEL"` {
		t.Fatal("message aliases caller")
	}
	if _, err := r.Output(ctx, a.launch.ID, 0); !errors.Is(err, ErrExecutorPanic) {
		t.Fatal(err)
	}
	if v := runtimeTestView(t, ctx, r, a.launch.ID); v.State != StateRunning {
		t.Fatal(v)
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, b)
	if err := r.Inject(ctx, a.launch.ID, nil); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
	runtimeTestClose(t, ctx, r)
}
