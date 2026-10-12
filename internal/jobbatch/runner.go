package jobbatch

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Runner serializes engine inputs; physical executor calls never run on its loop.
type Runner struct {
	commitHook          func(context.Context, Transition) error
	commitTimeout       time.Duration
	faulted             chan struct{}
	faultErr            error
	faultHandled        bool
	publicationQueue    []*runnerPublication
	commitInput         chan *runnerPublication
	commitResults       chan runnerCommitResult
	commitBusy          bool
	finalApplied        bool
	speculativeReleased map[ID]bool
	currentPublication  *runnerPublication
	engine              *Engine
	executor            Executor
	controlTimeout      time.Duration
	maxDepth, maxNodes  int
	clock               runnerClock
	containmentRequests chan *runnerRequest
	requests            chan *runnerRequest
	wake                chan struct{}
	inboxMu             sync.Mutex
	inbox               []runnerMessage
	exited              chan struct{}
	final               chan struct{}
	host                context.Context
	attempts            map[uint64]*runnerAttempt
	batches             map[ID]*Batch
	released            map[ID]bool
	closing             bool
	closeOnce           sync.Once
	requestHooks        *runnerRequestHooks

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
	runner  *Runner
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
	beforeRequestSubmit     func()
	beforeEventsContext     func()
	queued, accepted, reply func(*runnerRequest)
	// Instance-local scheduling seams; copied before goroutine publication.
	timeoutCancel         func(context.CancelFunc)
	controlReturned       func()
	donePublished         func(*Batch, Transition)
	beforeAppend          func(Transition)
	committed             func(*runnerRequest)
	commitReturned        func()
	beforeMessage         func(runnerMessage)
	afterMessage          func(runnerMessage)
	transitioned          func(Transition)
	closeBeforeFeedLock   func()
	beforeSettlementApply func()
	beforeStartedApply    func()
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
	if ctx == nil || cfg.Executor == nil || cfg.ControlTimeout <= 0 || cfg.RecordRetention <= 0 || clock == nil || (cfg.CommitHook != nil && cfg.CommitTimeout <= 0) {
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
		commitHook: cfg.CommitHook, commitTimeout: cfg.CommitTimeout, faulted: make(chan struct{}),
		engine: engine, executor: cfg.Executor, controlTimeout: cfg.ControlTimeout, requestHooks: hooks,
		maxDepth: cfg.Limits.MaxDepth, maxNodes: cfg.Limits.MaxNodes, clock: clock,
		containmentRequests: make(chan *runnerRequest), requests: make(chan *runnerRequest), wake: make(chan struct{}, 1), exited: make(chan struct{}), final: make(chan struct{}), host: ctx,
		attempts: make(map[uint64]*runnerAttempt), batches: make(map[ID]*Batch), released: make(map[ID]bool),
		views: make(map[ID]NodeView), snapshots: make(map[ID]BatchView), cleanupChanged: make(chan struct{}),
	}
	if cfg.CommitHook != nil {
		r.commitInput = make(chan *runnerPublication)
		r.commitResults = make(chan runnerCommitResult, 1)
		r.speculativeReleased = make(map[ID]bool)
		go r.committer()
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
	case <-b.runner.faulted:
		select {
		case <-b.done:
			return b.summary, nil
		default:
			return Summary{}, b.runner.FaultErr()
		}
	case <-ctx.Done():
		select {
		case <-b.done:
			return b.summary, nil
		default:
			return Summary{}, b.runner.resultAfterFault(ctx.Err())
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
		var cleanup <-chan struct{}
		if r.FaultErr() != nil {
			if !r.faultHandled {
				r.faultHandled = true
				r.failCommit("error")
			}
			drained, changed := r.faultDrained()
			cleanup = changed
			if drained {
				close(r.exited)
				r.cleanupNotify()
				return
			}
		}
		var send chan *runnerPublication
		var next *runnerPublication
		if r.FaultErr() == nil && r.commitHook != nil && !r.commitBusy && len(r.publicationQueue) > 0 {
			send = r.commitInput
			next = r.publicationQueue[0]
		}
		requests := r.requests
		if r.commitHook != nil && len(r.publicationQueue) >= runnerPublicationLimit {
			requests = nil
		}
		select {
		case <-cleanup:
		case <-hostDone:
			hostDone = nil
			if !r.closing {
				r.closing = true
				r.apply(Event{Type: EventHostClosing})
			}
		case send <- next:
			r.commitBusy = true
		case result := <-r.commitResults:
			r.commitBusy = false
			if result.err != "" {
				if !r.faultHandled {
					r.faultHandled = true
					r.failCommit(result.err)
				}
				continue
			}
			r.publicationQueue = r.publicationQueue[1:]
			r.publish(result.item)
		case q := <-r.containmentRequests:
			r.acceptRequest(q)
		case q := <-requests:
			r.acceptRequest(q)
		case <-r.wake:
			r.inboxMu.Lock()
			messages := r.inbox
			r.inbox = nil
			r.inboxMu.Unlock()
			for _, m := range messages {
				if r.requestHooks != nil && r.requestHooks.beforeMessage != nil {
					r.requestHooks.beforeMessage(m)
				}
				r.handleMessage(m)
				if r.requestHooks != nil && r.requestHooks.afterMessage != nil {
					r.requestHooks.afterMessage(m)
				}

				if r.isClosed() {
					break
				}
			}
		}
		if r.isClosed() && len(r.publicationQueue) == 0 {
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
	r.mu.Lock()
	if r.faultErr != nil {
		err := r.faultErr
		r.mu.Unlock()
		return Transition{}, err
	}
	if r.finalApplied {
		r.mu.Unlock()
		return Transition{}, ErrClosed
	}
	t, err := r.engine.Apply(e)
	r.mu.Unlock()
	if err != nil {
		return Transition{}, err
	}
	if r.commitHook != nil {
		return r.applyDurable(t)
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
			if r.commitHook == nil {
				r.dispatchStopCommand(cmd, t.Sequence)
			}
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
	validationErr error
	state         atomic.Uint32
	op            runnerRequestOp
	event         Event
	node          ID
	reply         chan runnerAnswer
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
	if err := r.FaultErr(); err != nil {
		return runnerAnswer{err: err}
	}
	if r.requestHooks != nil && r.requestHooks.beforeRequestSubmit != nil {
		r.requestHooks.beforeRequestSubmit()
	}
	q.reply = make(chan runnerAnswer, 1)
	if r.isClosed() {
		return r.afterClosed(q)
	}
	if err := ctx.Err(); err != nil {
		return runnerAnswer{err: r.resultAfterFault(err)}
	}
	requests := r.requests
	if r.commitHook != nil && q.event.Type == EventStopRequested {
		requests = r.containmentRequests
	}
	select {
	case requests <- q:
	case <-ctx.Done():
		q.state.CompareAndSwap(runnerQueued, runnerAbandoned)
		return runnerAnswer{err: r.resultAfterFault(ctx.Err())}
	case <-r.faulted:
		return runnerAnswer{err: r.FaultErr()}
	case <-r.exited:
		return r.afterClosed(q)
	}
	select {
	case <-r.faulted:
		select {
		case answer := <-q.reply:
			return answer
		default:
			return runnerAnswer{err: r.FaultErr()}
		}
	case a := <-q.reply:
		return a
	case <-ctx.Done():
		if q.state.CompareAndSwap(runnerQueued, runnerAbandoned) {
			return runnerAnswer{err: r.resultAfterFault(ctx.Err())}
		}
		return <-q.reply
	case <-r.exited:
		if q.state.CompareAndSwap(runnerQueued, runnerAbandoned) {
			return r.afterClosed(q)
		}
		return <-q.reply
	}
}

func (r *Runner) afterClosed(q *runnerRequest) (answer runnerAnswer) {
	defer func() {
		if err := r.resultAfterFault(answer.err); err != nil {
			answer = runnerAnswer{err: err}
		}
	}()
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
