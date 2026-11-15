package jobbatch

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// R1/E8/P2-e: executor owns partial-resource cleanup; Runtime must normalize
// panic/failure and free the slot. A dropped rejection strands the successor.
func TestRuntimeStartResourceCleanupAndSlot(t *testing.T) {
	for _, panics := range []bool{false, true} {
		name, code := "failure", CodeStartFailed
		if panics {
			name, code = "panic", CodeStartPanic
		}
		t.Run(name, func(t *testing.T) {
			r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
			b := runtimeTestSubmit(t, ctx, r, "R", runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask(), runtimeTestTask()))
			a := runtimeTestReceive(t, ctx, e.starts)
			cleaned := make(chan struct{})
			a.decision <- runtimeTestDecision{panicStart: panics, err: errors.New("PRIVATE_RESOURCE_FAILURE"), beforeReturn: func() {
				resource := make(chan struct{})
				defer func() { close(resource); close(cleaned) }()
				if panics {
					panic("PRIVATE_RESOURCE_PANIC")
				}
			}}
			rec := runtimeTestRecord(t, ctx, r, RecordStartRejected, a.launch.ID)
			if rec.Code != code {
				t.Fatal(rec)
			}
			select {
			case <-cleaned:
			default:
				t.Error("executor resource not cleaned before rejection")
			}
			next := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			if next.launch.ID != "R.2" {
				t.Fatal(next.launch)
			}
			a.report(Result{State: StateCompleted})
			runtimeTestFinish(t, ctx, r, next, StateCompleted)
			if s := runtimeTestWait(t, ctx, b); s.Counts.Failed != 1 || s.Counts.Completed != 1 {
				t.Fatal(s)
			}
			runtimeTestClose(t, ctx, r)
		})
	}
}

// R1/E8/V-c: catches settling early reports before Started and journaling rejected reports.
func TestRuntimeStartOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		decision runtimeTestDecision
		code     string
		state    State
	}{
		{"early", runtimeTestDecision{execution: &runtimeTestExecution{}, early: &Result{State: StateCompleted}}, "", StateCompleted},
		{"error", runtimeTestDecision{err: &runtimeTestCodeError{code: "missing_program"}, early: &Result{State: StateCompleted}}, "missing_program", StateFailed},
		{"nil", runtimeTestDecision{}, CodeStartNilExecution, StateFailed},
		{"panic", runtimeTestDecision{panicStart: true}, CodeStartPanic, StateFailed},
		{"reserved", runtimeTestDecision{err: &runtimeTestCodeError{code: CodeStopTimeout}}, CodeInvalid, StateFailed},
		{"long", runtimeTestDecision{err: &runtimeTestCodeError{code: strings.Repeat("a", 65)}}, CodeInvalid, StateFailed},
		{"private", runtimeTestDecision{err: &runtimeTestCodeError{code: "PRIVATE_SENTINEL/argv=x"}}, CodeInvalid, StateFailed},
		{"empty", runtimeTestDecision{err: &runtimeTestCodeError{}}, CodeStartFailed, StateFailed},
		{"code-panic", runtimeTestDecision{err: &runtimeTestCodeError{panicCode: true}}, CodeInvalid, StateFailed},
		{"as-panic", runtimeTestDecision{err: runtimeTestAsPanic{}}, CodeInvalid, StateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
			b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
			a := runtimeTestReceive(t, ctx, e.starts)
			a.decision <- tc.decision
			s := runtimeTestWait(t, ctx, b)
			if s.State != tc.state {
				t.Fatalf("summary: %+v", s)
			}
			v := runtimeTestView(t, ctx, r, a.launch.ID)
			if v.Outcome.Code != tc.code {
				t.Fatalf("outcome: %+v", v.Outcome)
			}
			a.report(Result{State: StateFailed, Code: "late"})
			runtimeTestClose(t, ctx, r)
			p := runtimeTestPage(t, ctx, r)
			started, settled, rejected := 0, 0, 0
			for _, rec := range p.Records {
				switch rec.Type {
				case RecordStarted:
					started++
				case RecordSettled:
					if started != 1 {
						t.Error("Settled preceded Started")
					}
					settled++
				case RecordStartRejected:
					rejected++
				}
				if strings.Contains(rec.Code, "PRIVATE") {
					t.Error("private code leaked")
				}
			}
			if tc.decision.execution != nil {
				if started != 1 || settled != 1 || rejected != 0 {
					t.Fatal(p)
				}
			} else if started != 0 || settled != 0 || rejected != 1 {
				t.Fatal(p)
			}
		})
	}
}

type runtimeTestCodeError struct {
	code      string
	panicCode bool
	capacity  bool
}

func (e *runtimeTestCodeError) Error() string { panic("error text must not be read") }
func (e *runtimeTestCodeError) Code() string {
	if e.panicCode {
		panic("code")
	}
	return e.code
}
func (e *runtimeTestCodeError) Is(target error) bool { return e.capacity && target == ErrCapacity }

type runtimeTestAsPanic struct{}

func (runtimeTestAsPanic) Error() string { panic("text") }
func (runtimeTestAsPanic) As(any) bool   { panic("As") }

// R1/R2: catches dropping Stop while Start is in flight and synthetic settlement on acknowledgement.
func TestRuntimeStopBeforeStartReturns(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
	a := runtimeTestReceive(t, ctx, e.starts)
	if err := r.Stop(ctx, a.launch.ID); err != nil {
		t.Fatal(err)
	}
	runtimeTestReceive(t, ctx, a.ctx.Done())
	// Observe inside Stop: Runtime legitimately cancels this context on return.
	entered := make(chan error, 1)
	a.decision <- runtimeTestDecision{execution: &runtimeTestExecution{stop: func(c context.Context) error { entered <- c.Err(); return nil }}}
	if err := runtimeTestReceive(t, ctx, entered); err != nil {
		t.Error("Stop inherited cancellation")
	}
	runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
	if v := runtimeTestView(t, ctx, r, a.launch.ID); v.State != StateCancelling {
		t.Fatal(v)
	}
	select {
	case <-b.Done():
		t.Fatal("ack settled work")
	default:
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	if s := runtimeTestWait(t, ctx, b); s.State != StateCancelled {
		t.Fatal(s)
	}
	runtimeTestClose(t, ctx, r)
}

// R3-one/R3-chain/R3-add/E1: catches consumer notice filtering and duplicate/out-of-order Add IDs.
func TestRuntimeNoticesAndAdd(t *testing.T) {
	for _, name := range []string{"one", "chain", "add", "helper-add"} {
		t.Run(name, func(t *testing.T) {
			r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
			spec := runtimeTestBatch(ModeParallel, NotifyEach, runtimeTestTask())
			want := []ID{"R"}
			if name == "chain" {
				spec = runtimeTestBatch(ModeParallel, NotifyEach, spec)
			}
			if name == "helper-add" {
				spec = OneItem(TaskSpec{Kind: "controlled"})
			}
			b := runtimeTestSubmit(t, ctx, r, "R", spec)
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			if name == "add" || name == "helper-add" {
				ids, err := r.Add(ctx, AddRequest{Parent: "R", Items: []Spec{runtimeTestTask()}, Ref: "second"})
				if err != nil || !reflect.DeepEqual(ids, []ID{"R.2"}) {
					t.Fatalf("IDs=%v err=%v", ids, err)
				}
				if name == "add" {
					want = []ID{"R.1", "R.2", "R"}
				}
			}
			runtimeTestFinish(t, ctx, r, a, StateCompleted)
			if name == "add" || name == "helper-add" {
				second := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
				runtimeTestFinish(t, ctx, r, second, StateCompleted)
			}
			runtimeTestWait(t, ctx, b)
			runtimeTestClose(t, ctx, r)
			if got := runtimeTestNotices(runtimeTestPage(t, ctx, r)); !reflect.DeepEqual(got, want) {
				t.Fatalf("notices=%v want=%v", got, want)
			}
		})
	}
}

// R5/R-a/R-d: catches false admission, terminal Stop records, and loss of cached Wait after Release.
func TestRuntimeNeverAdmittedSettlement(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
	aBatch := runtimeTestSubmit(t, ctx, r, "A", OneItem(TaskSpec{Kind: "controlled"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	b := runtimeTestSubmit(t, ctx, r, "B", runtimeTestBatch(ModeParallel, NotifyEach, runtimeTestBatch(ModeParallel, NotifyAll, runtimeTestTask()), runtimeTestTask()))
	if err := r.Stop(ctx, "B.1"); err != nil {
		t.Fatal(err)
	}
	if v := runtimeTestView(t, ctx, r, "B"); v.State != StatePending {
		t.Fatal(v)
	}
	if err := r.Stop(ctx, "B.2"); err != nil {
		t.Fatal(err)
	}
	s := runtimeTestWait(t, ctx, b)
	if s.State != StateCancelled || s.Counts.Cancelled != 2 {
		t.Fatal(s)
	}
	p := runtimeTestPage(t, ctx, r)
	if err := r.Stop(ctx, "B"); err != nil {
		t.Fatal(err)
	}
	if runtimeTestPage(t, ctx, r).Head != p.Head {
		t.Fatal("terminal Stop journaled")
	}
	if _, err := r.Add(ctx, AddRequest{Parent: "B", Items: []Spec{runtimeTestTask()}}); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
	if _, err := r.Output(ctx, "B.2", 0); !errors.Is(err, ErrNotStarted) {
		t.Fatal(err)
	}
	if err := r.Inject(ctx, "B.2", nil); !errors.Is(err, ErrTerminal) {
		t.Fatal(err)
	}
	if err := r.Release(ctx, "B"); err != nil {
		t.Fatal(err)
	}
	if err := r.Release(ctx, "B"); !errors.Is(err, ErrReleased) {
		t.Fatal(err)
	}
	if _, err := r.Snapshot(ctx, "B"); !errors.Is(err, ErrReleased) {
		t.Fatal(err)
	}
	if got := runtimeTestWait(t, ctx, b); got != s {
		t.Fatal(got)
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	runtimeTestWait(t, ctx, aBatch)
	runtimeTestClose(t, ctx, r)
}

// R5: acceptance wins after handoff; abandonment leaves no public input record.
// The committed seam runs after the actual capacity-one reply send. The append
// seam holds publication independently; a pre-reply hook alone cannot prove this.
func TestRuntimeCommittedReplyPublishesRecords(t *testing.T) {
	var r *Runner
	entered, release := make(chan struct{}), make(chan struct{})
	checked := make(chan bool, 1)
	hooks := &runnerRequestHooks{
		beforeAppend: func(tr Transition) {
			if tr.Event.Ref == "committed" {
				close(entered)
				<-release
			}
		},
		committed: func(q *runnerRequest) {
			if q.event.Ref != "committed" {
				return
			}
			p, _ := r.Events(runtimeTestCancelled(), 1)
			found := false
			for _, rec := range p.Records {
				if rec.Ref == "committed" {
					found = true
				}
			}
			checked <- found
		},
	}
	var ctx context.Context
	r, _, _, ctx = runtimeTestNew(t, 1, 128, hooks)
	done := make(chan error, 1)
	go func() {
		_, err := r.Submit(ctx, SubmitRequest{Root: "R", Spec: runtimeTestBatch(ModeParallel, NotifyAll), Ref: "committed"})
		done <- err
	}()
	runtimeTestReceive(t, ctx, entered)
	select {
	case found := <-checked:
		if !found {
			t.Error("actual committed reply preceded public record while append blocked")
		}
		close(release)
	default:
		close(release)
		if !runtimeTestReceive(t, ctx, checked) {
			t.Error("actual committed reply preceded public record")
		}
	}
	if err := runtimeTestReceive(t, ctx, done); err != nil {
		t.Fatal(err)
	}
	runtimeTestClose(t, ctx, r)
}

func TestRuntimeRequestCancellationWinner(t *testing.T) {
	for _, operation := range []string{"submit", "add"} {
		for _, phase := range []string{"queued", "accepted", "reply"} {
			t.Run(operation+"/"+phase, func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				hook := func(q *runnerRequest) {
					if q.event.Ref == "winner" {
						close(entered)
						<-release
					}
				}
				hooks := &runnerRequestHooks{}
				switch phase {
				case "queued":
					hooks.queued = hook
				case "accepted":
					hooks.accepted = hook
				case "reply":
					hooks.reply = hook
				}
				r, e, _, ctx := runtimeTestNew(t, 1, 128, hooks)
				b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
				a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
				callctx, cancel := context.WithCancel(ctx)
				defer cancel()
				type answer struct {
					ids   []ID
					batch *Batch
					err   error
				}
				done := make(chan answer, 1)
				go func() {
					if operation == "add" {
						ids, err := r.Add(callctx, AddRequest{Parent: "R", Items: []Spec{runtimeTestTask()}, Ref: "winner"})
						done <- answer{ids: ids, err: err}
					} else {
						batch, err := r.Submit(callctx, SubmitRequest{Root: "S", Spec: OneItem(TaskSpec{Kind: "controlled"}), Ref: "winner"})
						done <- answer{batch: batch, err: err}
					}
				}()
				runtimeTestReceive(t, ctx, entered)
				cancel()
				var got answer
				if phase == "queued" {
					// Wait for the abandonment CAS before allowing loop acceptance.
					got = runtimeTestReceive(t, ctx, done)
					if !errors.Is(got.err, context.Canceled) {
						t.Fatal(got)
					}
				} else {
					select {
					case <-done:
						t.Fatal("committed reply escaped barrier")
					default:
					}
				}
				if phase == "reply" {
					found := false
					for _, rec := range runtimeTestPage(t, ctx, r).Records {
						if rec.Ref == "winner" {
							found = true
						}
					}
					if !found {
						t.Fatal("reply preceded public record")
					}
				}
				close(release)
				if phase == "queued" {
					// View is a loop barrier: inspect abandonment before any pending
					// mutant work can turn cleanup into a missing-Start timeout.
					runtimeTestView(t, ctx, r, a.launch.ID)
					for _, rec := range runtimeTestPage(t, ctx, r).Records {
						if rec.Ref == "winner" {
							t.Fatal("abandoned request applied")
						}
					}
				}
				if phase != "queued" {
					got = runtimeTestReceive(t, ctx, done)
					if got.err != nil {
						t.Fatal(got.err)
					}
					if operation == "add" && !reflect.DeepEqual(got.ids, []ID{"R.2"}) {
						t.Fatal(got.ids)
					}
					if operation == "submit" && (got.batch == nil || got.batch.ID() != "S") {
						t.Fatal(got)
					}
				}
				runtimeTestFinish(t, ctx, r, a, StateCompleted)
				if phase != "queued" {
					second := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
					runtimeTestFinish(t, ctx, r, second, StateCompleted)
					if got.batch != nil {
						runtimeTestWait(t, ctx, got.batch)
					}
				}
				runtimeTestWait(t, ctx, b)
				runtimeTestClose(t, ctx, r)
				count := 0
				for _, rec := range runtimeTestPage(t, ctx, r).Records {
					if rec.Ref == "winner" {
						count++
					}
				}
				want := 1
				if phase == "queued" {
					want = 0
				}
				if count != want {
					t.Fatalf("input records=%d want=%d", count, want)
				}
			})
		}
	}
}
