package jobbatch

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

type (
	runtimeTestExecutor struct{ starts chan *runtimeTestAttempt }
	runtimeTestDecision struct {
		execution    Execution
		err          error
		early        *Result
		panicStart   bool
		beforeReturn func()
	}
	runtimeTestAttempt struct {
		launch   Launch
		ctx      context.Context
		report   func(Result)
		decision chan runtimeTestDecision
	}
)

func (e *runtimeTestExecutor) Start(ctx context.Context, l Launch, report func(Result)) (Execution, error) {
	a := &runtimeTestAttempt{l, ctx, report, make(chan runtimeTestDecision, 1)}
	e.starts <- a
	d := <-a.decision
	if d.early != nil {
		report(*d.early)
	}
	if d.beforeReturn != nil {
		d.beforeReturn()
	}
	if d.panicStart {
		panic("PRIVATE_SENTINEL")
	}
	return d.execution, d.err
}

type runtimeTestExecution struct {
	stop    func(context.Context) error
	read    func(context.Context, uint64) (OutputChunk, error)
	inject  func(context.Context, json.RawMessage) error
	dispose func(context.Context) error
}

func (e *runtimeTestExecution) Stop(ctx context.Context) error {
	if e.stop != nil {
		return e.stop(ctx)
	}
	return nil
}

func (e *runtimeTestExecution) ReadOutput(ctx context.Context, c uint64) (OutputChunk, error) {
	if e.read != nil {
		return e.read(ctx, c)
	}
	return OutputChunk{EOF: true, Next: c}, nil
}

func (e *runtimeTestExecution) Inject(ctx context.Context, m json.RawMessage) error {
	if e.inject != nil {
		return e.inject(ctx, m)
	}
	return nil
}

func (e *runtimeTestExecution) Dispose(ctx context.Context) error {
	if e.dispose != nil {
		return e.dispose(ctx)
	}
	return nil
}

type (
	runtimeTestClock struct {
		mu     sync.Mutex
		now    time.Duration
		timers []*runtimeTestTimer
		armed  chan *runtimeTestTimer
	}
	runtimeTestTimer struct {
		clock  *runtimeTestClock
		at     time.Duration
		f      func()
		active bool
	}
)

func (c *runtimeTestClock) AfterFunc(d time.Duration, f func()) runnerTimer {
	c.mu.Lock()
	timer := &runtimeTestTimer{c, c.now + d, f, true}
	c.timers = append(c.timers, timer)
	c.mu.Unlock()
	c.armed <- timer
	return timer
}

func (timer *runtimeTestTimer) Stop() bool {
	c := timer.clock
	c.mu.Lock()
	defer c.mu.Unlock()
	was := timer.active
	timer.active = false
	return was
}

func (c *runtimeTestClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now += d
	var callbacks []func()
	for _, timer := range c.timers {
		if timer.active && timer.at <= c.now {
			timer.active = false
			callbacks = append(callbacks, timer.f)
		}
	}
	c.mu.Unlock()
	for _, f := range callbacks {
		f()
	}
}

func runtimeTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func runtimeTestReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatal("barrier did not complete")
		var zero T
		return zero
	}
}

func runtimeTestNew(t *testing.T, limit int, retention int, hooks *runnerRequestHooks) (*Runner, *runtimeTestExecutor, *runtimeTestClock, context.Context) {
	t.Helper()
	ctx := runtimeTestContext(t)
	e := &runtimeTestExecutor{make(chan *runtimeTestAttempt, 32)}
	c := &runtimeTestClock{armed: make(chan *runtimeTestTimer, 128)}
	r, err := newRunnerWithClockAndHooks(context.Background(), RunnerConfig{Limits: Limits{MaxConcurrent: limit, MaxDepth: 16, MaxNodes: 128}, Executor: e, ControlTimeout: 50 * time.Second, RecordRetention: retention}, c, hooks)
	if err != nil {
		t.Fatal(err)
	}
	return r, e, c, ctx
}
func runtimeTestTask() Spec { return Spec{Task: &TaskSpec{Kind: "controlled"}} }
func runtimeTestBatch(mode Mode, notify Notify, items ...Spec) Spec {
	return Spec{Batch: &BatchSpec{Mode: mode, Notify: notify, Items: items}}
}

func runtimeTestSubmit(t *testing.T, ctx context.Context, r *Runner, id ID, s Spec) *Batch {
	t.Helper()
	b, err := r.Submit(ctx, SubmitRequest{Root: id, Spec: s, Ref: string(id)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runtimeTestRecord(t *testing.T, ctx context.Context, r *Runner, typ RecordType, id ID) Record {
	t.Helper()
	cursor := uint64(1)
	for {
		p, err := r.Events(ctx, cursor)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range p.Records {
			if rec.Type == typ && (id == "" || rec.Node == id) {
				return rec
			}
		}
		cursor = p.Next
	}
}

func runtimeTestAccept(t *testing.T, ctx context.Context, r *Runner, e *runtimeTestExecutor, x Execution) *runtimeTestAttempt {
	t.Helper()
	a := runtimeTestReceive(t, ctx, e.starts)
	a.decision <- runtimeTestDecision{execution: x}
	runtimeTestRecord(t, ctx, r, RecordStarted, a.launch.ID)
	return a
}

func runtimeTestFinish(t *testing.T, ctx context.Context, r *Runner, a *runtimeTestAttempt, state State) {
	t.Helper()
	a.report(Result{State: state})
	runtimeTestRecord(t, ctx, r, RecordSettled, a.launch.ID)
}

func runtimeTestWait(t *testing.T, ctx context.Context, b *Batch) Summary {
	t.Helper()
	s, err := b.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func runtimeTestClose(t *testing.T, ctx context.Context, r *Runner) {
	t.Helper()
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func runtimeTestView(t *testing.T, ctx context.Context, r *Runner, id ID) NodeView {
	t.Helper()
	v, err := r.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func runtimeTestPage(t *testing.T, ctx context.Context, r *Runner) EventPage {
	t.Helper()
	p, err := r.Events(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runtimeTestNotices(p EventPage) []ID {
	var ids []ID
	for _, rec := range p.Records {
		if rec.Type == RecordNotice {
			ids = append(ids, rec.Node)
		}
	}
	return ids
}

func runtimeTestCancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
