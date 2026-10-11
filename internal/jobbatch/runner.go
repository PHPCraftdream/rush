package jobbatch

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Runner serializes engine inputs; physical executor calls never run on its loop.
type Runner struct {
	engine             *Engine
	executor           Executor
	controlTimeout     time.Duration
	maxDepth, maxNodes int
	clock              runnerClock
	requests           chan *runnerRequest
	wake               chan struct{}
	inboxMu            sync.Mutex
	inbox              []runnerMessage
	exited             chan struct{}
	final              chan struct{}
	host               context.Context
	attempts           map[uint64]*runnerAttempt
	batches            map[ID]*Batch
	released           map[ID]bool
	closing            bool
	closeOnce          sync.Once
	requestHooks       *runnerRequestHooks

	// Published state and lifetime cleanup survive the engine loop.
	mu             sync.Mutex
	views          map[ID]NodeView
	snapshots      map[ID]BatchView
	handles        []*runnerHandle
	cleanupChanged chan struct{}
	closed         bool
	feed           runnerFeed
}

// Batch retains its immutable settlement even after Release or Runner closure.
type Batch struct {
	id      ID
	done    chan struct{}
	summary Summary // written before done closes, then immutable
}

// runnerClock is an instance-local injection seam, not part of RunnerConfig.
// Tests construct via newRunnerWithClock before any goroutine can use the clock.
type (
	runnerClock interface {
		AfterFunc(time.Duration, func()) runnerTimer
	}
	runnerTimer     interface{ Stop() bool }
	runnerRealClock struct{}
)

func (runnerRealClock) AfterFunc(d time.Duration, f func()) runnerTimer { return time.AfterFunc(d, f) }

func NewRunner(ctx context.Context, cfg RunnerConfig) (*Runner, error) {
	return newRunnerWithClock(ctx, cfg, runnerRealClock{})
}

type runnerRequestHooks struct {
	queued, accepted, reply func(*runnerRequest)
	// Instance-local scheduling seams; copied before goroutine publication.
	timeoutCancel   func(context.CancelFunc)
	controlReturned func()
	donePublished   func(*Batch, Transition)
	beforeAppend    func(Transition)
	committed       func(*runnerRequest)
}

func (r *Runner) cancelControlTimeout(cancel context.CancelFunc) {
	if r.requestHooks != nil && r.requestHooks.timeoutCancel != nil {
		r.requestHooks.timeoutCancel(cancel)
	} else {
		cancel()
	}
}

func (r *Runner) controlReturned() {
	if r.requestHooks != nil && r.requestHooks.controlReturned != nil {
		r.requestHooks.controlReturned()
	}
}

func newRunnerWithClock(ctx context.Context, cfg RunnerConfig, clock runnerClock) (*Runner, error) {
	return newRunnerWithClockAndHooks(ctx, cfg, clock, nil)
}

func newRunnerWithClockAndHooks(ctx context.Context, cfg RunnerConfig, clock runnerClock, hooks *runnerRequestHooks) (*Runner, error) {
	if ctx == nil || cfg.Executor == nil || cfg.ControlTimeout <= 0 || cfg.RecordRetention <= 0 || clock == nil {
		return nil, ErrInvalidSpec
	}
	// The Engine alone owns copying/validating Limits, including ByGroup.
	engine, err := NewEngine(cfg.Limits)
	if err != nil {
		return nil, err
	}
	// Copy the instance hooks before publication; callers cannot replace them live.
	if hooks != nil {
		copyHooks := *hooks
		hooks = &copyHooks
	}
	r := &Runner{
		engine: engine, executor: cfg.Executor, controlTimeout: cfg.ControlTimeout, requestHooks: hooks,
		maxDepth: cfg.Limits.MaxDepth, maxNodes: cfg.Limits.MaxNodes, clock: clock,
		requests: make(chan *runnerRequest), wake: make(chan struct{}, 1), exited: make(chan struct{}), final: make(chan struct{}), host: ctx,
		attempts: make(map[uint64]*runnerAttempt), batches: make(map[ID]*Batch), released: make(map[ID]bool),
		views: make(map[ID]NodeView), snapshots: make(map[ID]BatchView), cleanupChanged: make(chan struct{}),
	}
	r.feed = runnerFeed{retention: cfg.RecordRetention, changed: make(chan struct{})}
	go r.loop()
	return r, nil
}

func (b *Batch) ID() ID                { return b.id }
func (b *Batch) Done() <-chan struct{} { return b.done }
func (b *Batch) Wait(ctx context.Context) (Summary, error) {
	select {
	case <-b.done:
		return b.summary, nil
	default:
	}
	select {
	case <-b.done:
		return b.summary, nil
	case <-ctx.Done():
		select {
		case <-b.done:
			return b.summary, nil
		default:
			return Summary{}, ctx.Err()
		}
	}
}

// enqueue never waits for the loop (including synchronous Start callbacks).
func (r *Runner) enqueue(m runnerMessage) {
	select {
	case <-r.exited:
		return
	default:
	}
	r.inboxMu.Lock()
	select {
	case <-r.exited:
		r.inboxMu.Unlock()
		return
	default:
	}
	r.inbox = append(r.inbox, m)
	r.inboxMu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) loop() {
	hostDone := r.host.Done()
	for {
		select {
		case <-hostDone:
			hostDone = nil
			if !r.closing {
				r.closing = true
				r.apply(Event{Type: EventHostClosing})
			}
		case q := <-r.requests:
			if r.requestHooks != nil && r.requestHooks.queued != nil {
				r.requestHooks.queued(q)
			}
			if q.state.CompareAndSwap(runnerQueued, runnerAccepted) {
				if r.requestHooks != nil && r.requestHooks.accepted != nil {
					r.requestHooks.accepted(q)
				}
				answer := r.handleRequest(q)
				if r.requestHooks != nil && r.requestHooks.reply != nil {
					r.requestHooks.reply(q)
				}
				q.reply <- answer // a private capacity-one committed reply slot
				if r.requestHooks != nil && r.requestHooks.committed != nil {
					r.requestHooks.committed(q)
				}
			}
		case <-r.wake:
			r.inboxMu.Lock()
			messages := r.inbox
			r.inbox = nil
			r.inboxMu.Unlock()
			for _, m := range messages {
				r.handleMessage(m)
				if r.isClosed() {
					break
				}
			}
		}
		if r.isClosed() {
			// §3.4 preserves frozen views, not attempts or Launch payloads.
			clear(r.attempts)
			clear(r.batches)
			clear(r.feed.known)
			// No further engine access after this point. Lifetime locks remain live.
			close(r.exited)
			r.inboxMu.Lock()
			r.inbox = nil
			r.inboxMu.Unlock()
			r.mu.Lock()
			handles := append([]*runnerHandle(nil), r.handles...)
			r.mu.Unlock()
			for _, h := range handles {
				h.giveUp()
			}
			close(r.final)
			return
		}
	}
}

func (r *Runner) isClosed() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.closed }

// apply publishes records before settlement channels or committed replies.
func (r *Runner) apply(e Event) (Transition, error) {
	t, err := r.engine.Apply(e)
	if err != nil {
		return Transition{}, err
	}
	r.mu.Lock()
	for _, v := range t.Changed {
		r.views[v.ID] = v
	}
	r.mu.Unlock()
	r.appendTransition(t)
	if e.Type == EventRelease {
		r.mu.Lock()
		r.released[e.Root] = true
		// §4.5: only a root tombstone and cleanup metadata survive Release.
		for id, v := range r.views {
			if v.Root == e.Root {
				delete(r.attempts, v.Token)
				delete(r.views, id)
				delete(r.snapshots, id)
				delete(r.feed.known, id)
			}
		}
		delete(r.batches, e.Root)
		handles := append([]*runnerHandle(nil), r.handles...)
		r.mu.Unlock()
		for _, h := range handles {
			if h.root == e.Root {
				h.giveUp()
			}
		}
	}
	// Retire timers and controls on accepted rejection/termination, not callbacks.
	if e.Type == EventStartRejected || e.Type == EventSettled {
		if a := r.attempts[e.Token]; a != nil {
			if a.timer != nil {
				a.timer.Stop()
			}
			a.cancel()
			a.retired = true
			a.launch.Task.Payload = nil
			a.timer = nil
			if e.Type == EventStartRejected {
				delete(r.attempts, e.Token)
			}
			if a.handle != nil {
				a.handle.terminate()
			}
		}
	}
	if t.Final {
		r.mu.Lock()
		for id, v := range r.views {
			if v.Kind == KindBatch && !r.released[v.Root] {
				if s, ok := r.engine.Snapshot(id); ok {
					r.snapshots[id] = runnerCopySnapshot(s)
				}
			}
		}
		r.mu.Unlock()
	}
	for _, s := range t.SettledRoots {
		if b := r.batches[s.Root]; b != nil {
			b.summary = s
			close(b.done)
			if r.requestHooks != nil && r.requestHooks.donePublished != nil {
				r.requestHooks.donePublished(b, t)
			}
		}
	}
	for _, cmd := range t.Commands {
		switch cmd.Type {
		case CommandStartLeaf:
			r.dispatchStart(cmd.Launch)
		case CommandStopLeaf:
			r.dispatchStopCommand(cmd)
		}
	}
	if t.Final {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
	}
	return t, nil
}

const (
	runnerQueued uint32 = iota
	runnerAccepted
	runnerAbandoned
)

type runnerRequest struct {
	state atomic.Uint32
	op    runnerRequestOp
	event Event
	node  ID
	reply chan runnerAnswer
}
type runnerRequestOp uint8

const (
	runnerRequestEvent runnerRequestOp = iota
	runnerRequestView
	runnerRequestSnapshot
	runnerRequestOutput
	runnerRequestInject
	runnerRequestClose
)

type runnerAnswer struct {
	batch    *Batch
	ids      []ID
	view     NodeView
	snapshot BatchView
	handle   *runnerHandle
	err      error
}

func (r *Runner) request(ctx context.Context, q *runnerRequest) runnerAnswer {
	q.reply = make(chan runnerAnswer, 1)
	if r.isClosed() {
		return r.afterClosed(q)
	}
	if err := ctx.Err(); err != nil {
		return runnerAnswer{err: err}
	}
	select {
	case r.requests <- q:
	case <-ctx.Done():
		q.state.CompareAndSwap(runnerQueued, runnerAbandoned)
		return runnerAnswer{err: ctx.Err()}
	case <-r.exited:
		return r.afterClosed(q)
	}
	select {
	case a := <-q.reply:
		return a
	case <-ctx.Done():
		if q.state.CompareAndSwap(runnerQueued, runnerAbandoned) {
			return runnerAnswer{err: ctx.Err()}
		}
		return <-q.reply
	case <-r.exited:
		if q.state.CompareAndSwap(runnerQueued, runnerAbandoned) {
			return r.afterClosed(q)
		}
		return <-q.reply
	}
}

func (r *Runner) afterClosed(q *runnerRequest) runnerAnswer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if q.op != runnerRequestView && q.op != runnerRequestSnapshot {
		return runnerAnswer{err: ErrClosed}
	}
	if r.releasedNode(q.node) {
		return runnerAnswer{err: ErrReleased}
	}
	v, ok := r.views[q.node]
	if !ok {
		return runnerAnswer{err: ErrUnknownNode}
	}
	if r.released[v.Root] {
		return runnerAnswer{err: ErrReleased}
	}
	if q.op == runnerRequestView {
		return runnerAnswer{view: v}
	}
	s, ok := r.snapshots[q.node]
	if !ok {
		return runnerAnswer{err: ErrUnsupported}
	}
	return runnerAnswer{snapshot: runnerCopySnapshot(s)}
}
