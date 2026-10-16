package jobbatch

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// R2/E8: catches early timeout settlement, replaced stop causes, and live stale attempt timers.
func TestRuntimeVirtualDeadlines(t *testing.T) {
	for _, winner := range []string{"timeout", "stop", "natural"} {
		t.Run(winner, func(t *testing.T) {
			r, e, c, ctx := runtimeTestNew(t, 1, 128, nil)
			b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled", Timeout: 10 * time.Second}))
			runtimeTestReceive(t, ctx, c.armed)
			a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
			switch winner {
			case "natural":
				runtimeTestFinish(t, ctx, r, a, StateCompleted)
			case "stop":
				if err := r.Stop(ctx, a.launch.ID); err != nil {
					t.Fatal(err)
				}
				runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
			}
			c.advance(10 * time.Second)
			if winner == "timeout" {
				runtimeTestRecord(t, ctx, r, RecordDeadlineExpired, a.launch.ID)
				if v := runtimeTestView(t, ctx, r, a.launch.ID); v.State != StateCancelling || v.Cause != CauseTimeout {
					t.Fatal(v)
				}
				select {
				case <-b.Done():
					t.Fatal("deadline settled execution")
				default:
				}
			}
			if winner != "natural" {
				runtimeTestFinish(t, ctx, r, a, StateCompleted)
			}
			runtimeTestWait(t, ctx, b)
			runtimeTestClose(t, ctx, r)
			v := runtimeTestView(t, ctx, r, a.launch.ID)
			want := map[string]State{"timeout": StateTimedOut, "stop": StateCancelled, "natural": StateCompleted}[winner]
			if v.State != want || v.Outcome.State != StateCompleted {
				t.Fatal(v)
			}
		})
	}
}

// R1/R2/E8/V-c: catches capacity Code reads, rejected-report replay, stale deadlines, and canceled Wait stopping work.
func TestRuntimeCapacityRetryTimer(t *testing.T) {
	r, e, c, ctx := runtimeTestNew(t, 1, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled", Timeout: 10 * time.Second}))
	oldTimer := runtimeTestReceive(t, ctx, c.armed)
	old := runtimeTestReceive(t, ctx, e.starts)
	old.decision <- runtimeTestDecision{err: &runtimeTestCodeError{capacity: true, panicCode: true}, early: &Result{State: StateCompleted}}
	rec := runtimeTestRecord(t, ctx, r, RecordStartRejected, old.launch.ID)
	if rec.Reason != RejectCapacity || rec.Code != "" {
		t.Fatal(rec)
	}
	if _, err := b.Wait(runtimeTestCancelled()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := r.CapacityAvailable(ctx, ""); err != nil {
		t.Fatal(err)
	}
	runtimeTestReceive(t, ctx, c.armed)
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	if a.launch.Token == old.launch.Token {
		t.Fatal("reused token")
	}
	oldTimer.f()
	old.report(Result{State: StateFailed})
	// An accepted public request fences the enqueued stale messages via a future feed append.
	if err := r.CapacityAvailable(ctx, ""); err != nil {
		t.Fatal(err)
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	if s := runtimeTestWait(t, ctx, b); s.State != StateCompleted {
		t.Fatal(s)
	}
	runtimeTestClose(t, ctx, r)
	p := runtimeTestPage(t, ctx, r)
	for _, v := range p.Records {
		if v.Type == RecordDeadlineExpired || (v.Type == RecordSettled && v.Token == old.launch.Token) {
			t.Fatal(v)
		}
	}
}

// C-b/R5/W-b: catches overlapping physical Stops, queued timer start, late duplicate control and premature slot release.
func TestRuntimeSerializedStopRetry(t *testing.T) {
	r, e, c, ctx := runtimeTestNew(t, 1, 256, nil)
	entered := make(chan context.Context, 2)
	returns := make(chan error, 2)
	x := &runtimeTestExecution{stop: func(c context.Context) error { entered <- c; return <-returns }}
	aBatch := runtimeTestSubmit(t, ctx, r, "A", OneItem(TaskSpec{Kind: "controlled"}))
	a := runtimeTestAccept(t, ctx, r, e, x)
	b := runtimeTestSubmit(t, ctx, r, "B", OneItem(TaskSpec{Kind: "controlled"}))
	if err := r.Stop(ctx, a.launch.ID); err != nil {
		t.Fatal(err)
	}
	first := runtimeTestReceive(t, ctx, entered)
	runtimeTestReceive(t, ctx, c.armed)
	c.advance(50 * time.Second)
	rec := runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
	if rec.Code != CodeStopTimeout {
		t.Fatal(rec)
	}
	runtimeTestReceive(t, ctx, first.Done())
	if err := r.Stop(ctx, a.launch.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(ctx, a.launch.ID); err != nil {
		t.Fatal(err)
	}
	c.advance(100 * time.Second)
	v := runtimeTestView(t, ctx, r, a.launch.ID)
	if !v.ControlPending || v.ControlError != CodeStopTimeout {
		t.Fatal(v)
	}
	select {
	case <-entered:
		t.Fatal("physical Stops overlapped")
	default:
	}
	select {
	case <-c.armed:
		t.Fatal("queued Stop armed timer")
	default:
	}
	if v := runtimeTestView(t, ctx, r, "B.1"); v.State != StatePending || v.Attempts != 0 {
		t.Fatal(v)
	}
	returns <- nil
	second := runtimeTestReceive(t, ctx, entered)
	if second.Err() != nil {
		t.Fatal("queued timeout started before dispatch")
	}
	runtimeTestReceive(t, ctx, c.armed)
	returns <- errors.New("PRIVATE_SENTINEL")
	cursor := rec.Cursor + 1
	for {
		p, err := r.Events(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, rec := range p.Records {
			if rec.Type == RecordControl {
				if rec.Code != CodeStopFailed {
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
	if v := runtimeTestView(t, ctx, r, "B.1"); v.State != StatePending {
		t.Fatal(v)
	}
	runtimeTestFinish(t, ctx, r, a, StateCancelled)
	runtimeTestWait(t, ctx, aBatch)
	next := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	runtimeTestFinish(t, ctx, r, next, StateCompleted)
	runtimeTestWait(t, ctx, b)
	runtimeTestClose(t, ctx, r)
	controls, commands, requests := 0, 0, 0
	for _, rec := range runtimeTestPage(t, ctx, r).Records {
		if rec.Node != a.launch.ID {
			continue
		}
		if rec.Type == RecordControl {
			controls++
		}
		if rec.Type == RecordCommand && rec.Command == CommandStopLeaf {
			commands++
		}
		if rec.Type == RecordStopRequested {
			requests++
		}
	}
	if controls != 2 || commands != 2 || requests != 3 {
		t.Fatalf("controls=%d commands=%d requests=%d", controls, commands, requests)
	}
}

// C-a/C-d/R4: catches disposal crossing held read/Stop pins, lost Release cleanup, and post-F late control records.
func TestRuntimePinsBlockDisposal(t *testing.T) {
	for _, pin := range []string{"read-release", "stop-release", "read-final", "stop-final"} {
		t.Run(pin, func(t *testing.T) {
			r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
			entered, unblock, disposed := make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1)
			x := &runtimeTestExecution{dispose: func(context.Context) error { disposed <- struct{}{}; return nil }}
			if pin == "read-release" || pin == "read-final" {
				x.read = func(context.Context, uint64) (OutputChunk, error) {
					entered <- struct{}{}
					<-unblock
					return OutputChunk{EOF: true}, nil
				}
			} else {
				x.stop = func(context.Context) error { entered <- struct{}{}; <-unblock; return nil }
			}
			b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
			a := runtimeTestAccept(t, ctx, r, e, x)
			readDone := make(chan error, 1)
			if x.read != nil {
				go func() { _, err := r.Output(ctx, a.launch.ID, 0); readDone <- err }()
			} else if err := r.Stop(ctx, a.launch.ID); err != nil {
				t.Fatal(err)
			}
			runtimeTestReceive(t, ctx, entered)
			runtimeTestFinish(t, ctx, r, a, StateCancelled)
			summary := runtimeTestWait(t, ctx, b)
			if pin == "read-release" || pin == "stop-release" {
				if err := r.Release(ctx, "R"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Snapshot(ctx, "R"); !errors.Is(err, ErrReleased) {
					t.Fatal(err)
				}
			} else {
				closeDone := make(chan error, 1)
				go func() { closeDone <- r.Close(runtimeTestCancelled()) }()
				runtimeTestReceive(t, ctx, closeDone)
				runtimeTestRecord(t, ctx, r, RecordClosed, "")
				err := r.Close(runtimeTestCancelled())
				var d *DisposeError
				if !errors.As(err, &d) || len(d.Failures) != 1 || d.Failures[0].Code != CodeDisposePending {
					t.Fatalf("pending=%v", err)
				}
			}
			select {
			case <-disposed:
				t.Fatal("disposed across pin")
			default:
			}
			if got := runtimeTestWait(t, ctx, b); got != summary {
				t.Fatal(got)
			}
			close(unblock)
			if x.read != nil {
				if err := runtimeTestReceive(t, ctx, readDone); err != nil {
					t.Fatal(err)
				}
			}
			runtimeTestReceive(t, ctx, disposed)
			runtimeTestClose(t, ctx, r)
			for _, rec := range runtimeTestPage(t, ctx, r).Records {
				if rec.Type == RecordControl {
					t.Fatal("late Stop produced control", rec)
				}
			}
		})
	}
}

// C-d/F-b: catches disposal retries and late returns overwriting a logical timeout or frozen feed.
func TestRuntimeDisposeResults(t *testing.T) {
	for _, mode := range []string{"ok", "failure", "panic", "timeout", "pending"} {
		t.Run(mode, func(t *testing.T) {
			r, e, c, ctx := runtimeTestNew(t, 1, 128, nil)
			entered, unblock, returned := make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1)
			x := &runtimeTestExecution{dispose: func(c context.Context) error {
				if c.Err() != nil {
					t.Error("detached disposal pre-cancelled")
				}
				entered <- struct{}{}
				<-unblock
				defer func() { returned <- struct{}{} }()
				if mode == "panic" {
					panic("PRIVATE_SENTINEL")
				}
				if mode == "failure" {
					return errors.New("PRIVATE_SENTINEL")
				}
				return nil
			}}
			b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
			a := runtimeTestAccept(t, ctx, r, e, x)
			runtimeTestFinish(t, ctx, r, a, StateCompleted)
			summary := runtimeTestWait(t, ctx, b)
			done := make(chan error, 1)
			go func() { done <- r.Close(ctx) }()
			runtimeTestReceive(t, ctx, entered)
			runtimeTestReceive(t, ctx, c.armed)
			frozen := runtimeTestPage(t, ctx, r)
			if mode == "pending" {
				err := r.Close(runtimeTestCancelled())
				var d *DisposeError
				if !errors.As(err, &d) || d.Failures[0].Code != CodeDisposePending {
					t.Fatal(err)
				}
			}
			if mode == "timeout" {
				c.advance(50 * time.Second)
			} else {
				close(unblock)
			}
			err := runtimeTestReceive(t, ctx, done)
			want := map[string]string{"failure": CodeDisposeFailed, "panic": CodeDisposePanic, "timeout": CodeDisposeTimeout}[mode]
			if want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var d *DisposeError
				if !errors.As(err, &d) || len(d.Failures) != 1 || d.Failures[0].Node != a.launch.ID || d.Failures[0].Token != a.launch.Token || d.Failures[0].Code != want {
					t.Fatal(err)
				}
			}
			if mode == "timeout" {
				close(unblock)
			}
			runtimeTestReceive(t, ctx, returned)
			repeat := r.Close(ctx)
			if !reflect.DeepEqual(err, repeat) {
				t.Fatalf("unstable cleanup: %v / %v", err, repeat)
			}
			if got := runtimeTestPage(t, ctx, r); !reflect.DeepEqual(got, frozen) {
				t.Fatal("cleanup altered feed")
			}
			if got := runtimeTestWait(t, ctx, b); got != summary {
				t.Fatal(got)
			}
			select {
			case <-entered:
				t.Fatal("Dispose retried")
			default:
			}
		})
	}
}

// R2/F-e/P2-e: host cancellation alone accepts one HostClosing. A failed
// control cannot settle work or be retried by later public Close calls.
func TestRuntimeHostCancellationFailedControlDrains(t *testing.T) {
	ctx := runtimeTestContext(t)
	host, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &runtimeTestExecutor{starts: make(chan *runtimeTestAttempt, 32)}
	returned := make(chan struct{}, 8)
	calls := make(chan struct{}, 8)
	r, err := newRunnerWithClockAndHooks(host, RunnerConfig{Limits: Limits{MaxConcurrent: 1, MaxDepth: 8, MaxNodes: 64}, Executor: e, ControlTimeout: time.Second, RecordRetention: 128}, runnerRealClock{}, &runnerRequestHooks{controlReturned: func() { returned <- struct{}{} }})
	if err != nil {
		t.Fatal(err)
	}
	b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{stop: func(c context.Context) error {
		calls <- struct{}{}
		if c.Err() != nil {
			t.Error("control inherited cancelled host")
		}
		return errors.New("PRIVATE_STOP_FAILURE")
	}})
	cancel()
	runtimeTestRecord(t, ctx, r, RecordHostClosing, "")
	if rec := runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID); rec.Code != CodeStopFailed {
		t.Fatal(rec)
	}
	runtimeTestReceive(t, ctx, returned)
	if v := runtimeTestView(t, ctx, r, a.launch.ID); v.State != StateCancelling || v.Cause != CauseHostClosing {
		t.Fatal(v)
	}
	select {
	case <-b.Done():
		t.Fatal("failed control synthesized settlement")
	default:
	}
	for range 2 {
		if err := r.Close(runtimeTestCancelled()); !errors.Is(err, ErrCloseTimeout) {
			t.Fatal(err)
		}
		// A loop request follows the queued Close intent, without relying on sleep.
		if _, err := r.View(ctx, a.launch.ID); err != nil {
			t.Fatal(err)
		}
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	if s := runtimeTestWait(t, ctx, b); s.State != StateInterrupted {
		t.Fatal(s)
	}
	runtimeTestClose(t, ctx, r)
	closing, controls, settled := 0, 0, 0
	for _, rec := range runtimeTestPage(t, ctx, r).Records {
		switch rec.Type {
		case RecordHostClosing:
			closing++
		case RecordControl:
			controls++
		case RecordSettled:
			settled++
		}
	}
	if closing != 1 || controls != 1 || settled != 1 || len(calls) != 1 {
		t.Fatalf("closing=%d controls=%d settled=%d stop calls=%d", closing, controls, settled, len(calls))
	}
}

// R2/F-e/F-f: catches losing first Close intent when its waiter is already canceled.
func TestRuntimeCancelledCloseStillDrains(t *testing.T) {
	r, e, _, ctx := runtimeTestNew(t, 1, 128, nil)
	b := runtimeTestSubmit(t, ctx, r, "R", OneItem(TaskSpec{Kind: "controlled"}))
	a := runtimeTestAccept(t, ctx, r, e, &runtimeTestExecution{})
	if err := r.Close(runtimeTestCancelled()); !errors.Is(err, ErrCloseTimeout) {
		t.Fatal(err)
	}
	runtimeTestRecord(t, ctx, r, RecordHostClosing, "")
	runtimeTestRecord(t, ctx, r, RecordControl, a.launch.ID)
	if _, err := r.Submit(ctx, SubmitRequest{Root: "late", Spec: OneItem(TaskSpec{Kind: "controlled"})}); !errors.Is(err, ErrClosing) {
		t.Fatal(err)
	}
	if v := runtimeTestView(t, ctx, r, a.launch.ID); v.State != StateCancelling || v.Cause != CauseHostClosing {
		t.Fatal(v)
	}
	select {
	case <-b.Done():
		t.Fatal("Close synthesized termination")
	default:
	}
	runtimeTestFinish(t, ctx, r, a, StateCompleted)
	if s := runtimeTestWait(t, ctx, b); s.State != StateInterrupted {
		t.Fatal(s)
	}
	runtimeTestClose(t, ctx, r)
	count := 0
	for _, rec := range runtimeTestPage(t, ctx, r).Records {
		if rec.Type == RecordHostClosing {
			count++
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
}
