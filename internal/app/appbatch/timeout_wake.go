package appbatch

import (
	"context"
	"errors"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

const TimeoutWakeQueueSize = 32

// pwBatchWakeRequest is one queued wake: either a resolved owner (the
// timeout check-in path) or a root whose owner is resolved from the durable
// row at dispatch time (the ordinary completion path).
type pwBatchWakeRequest struct {
	owner string
	root  string
}

func (r pwBatchWakeRequest) key() string {
	if r.root != "" {
		return "root:" + r.root
	}
	return "owner:" + r.owner
}

// CompletionRootOwner resolves the owning session of a committed
// batch root from the durable row (batch_nodes.owner_session_id), never
// from hook or host identity. The writer is read at call time so a resolver
// created before the App's writer is bound still uses the live pool.
// apiServiceOwner reports whether the owner is an API service session: such a
// session holds batch roots for a token and never runs a conversational turn,
// so no batch event may wake its model.
func apiServiceOwner(p BatchCLIParams) func(context.Context, string) bool {
	return func(ctx context.Context, owner string) bool {
		if p.Sessions == nil || p.Sessions() == nil {
			return false
		}
		item, err := p.Sessions().Get(ctx, owner)
		return err == nil && item.Origin == message.OriginAPIService
	}
}

func CompletionRootOwner(p BatchCLIParams) func(context.Context, string) (string, error) {
	return func(ctx context.Context, root string) (string, error) {
		if p.Owner == nil {
			return "", errors.New("batch completion wake: nil app")
		}
		w := CutoverWriter(p)
		if w == nil {
			return "", errors.New("batch completion wake: no cutover writer")
		}
		return session.NewBatchStore(w).RootOwner(ctx, root)
	}
}

type batchTimeoutWakeRuntime struct {
	checkin interface {
		Start()
		Stop()
	}
	dispatch *TimeoutWakeDispatcher
}

func (r batchTimeoutWakeRuntime) Start() {
	if r.checkin != nil {
		r.checkin.Start()
	}
}

func (r batchTimeoutWakeRuntime) Stop() {
	if r.checkin != nil {
		r.checkin.Stop()
	}
	if r.dispatch != nil {
		r.dispatch.Stop()
	}
}

type TimeoutWakeDispatcher struct {
	mu        sync.Mutex
	coord     agent.Coordinator
	queue     chan pwBatchWakeRequest
	rootOwner func(context.Context, string) (string, error)
	// skipOwner, when set, vetoes a wake for owners that must never run a
	// model turn (API service sessions).
	skipOwner func(context.Context, string) bool
	// queued holds the key of every request currently sitting in the queue
	// or the unattached pending buffer, so a repeated commit or a second
	// pull of the same debt cannot enqueue twice while a wake is in flight.
	queued map[string]bool
	// pend holds requests popped before a coordinator attached (a
	// completion may commit before the detached started acknowledgement
	// attaches the coordinator); Attach replays them so the early
	// unsuccessful wake never permanently loses the later eligible debt.
	pend   []pwBatchWakeRequest
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	stop   sync.Once
	start  sync.Once
}

func NewTimeoutWakeDispatcher() *TimeoutWakeDispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &TimeoutWakeDispatcher{
		queue:  make(chan pwBatchWakeRequest, TimeoutWakeQueueSize),
		queued: make(map[string]bool),
		ctx:    ctx, cancel: cancel, done: make(chan struct{}),
	}
}

func (d *TimeoutWakeDispatcher) Start() {
	if d == nil {
		return
	}
	d.start.Do(func() {
		select {
		case <-d.ctx.Done():
			close(d.done)
		default:
			go d.run()
		}
	})
}

func (d *TimeoutWakeDispatcher) Attach(c agent.Coordinator) {
	if d == nil || c == nil {
		return
	}
	d.mu.Lock()
	d.coord = c
	pend := d.pend
	d.pend = nil
	d.mu.Unlock()
	d.Start()
	for _, req := range pend {
		select {
		case d.queue <- req:
		default:
			// Bounded: drop the replay overflow; the durable debt is
			// level-readable and the next eligible recheck still wakes.
			d.release(req)
		}
	}
}

// Wake queues a timeout check-in wake for an already-resolved owner.
func (d *TimeoutWakeDispatcher) Wake(_ context.Context, owner string) {
	d.enqueue(pwBatchWakeRequest{owner: owner})
}

// WakeCompletion queues the ordinary-completion wake for one committed
// root; the owner is resolved from the durable row at dispatch time. It
// never blocks and never fails its caller: the commit stack only enqueues.
func (d *TimeoutWakeDispatcher) WakeCompletion(_ context.Context, root string) {
	d.enqueue(pwBatchWakeRequest{root: root})
}

func (d *TimeoutWakeDispatcher) enqueue(req pwBatchWakeRequest) {
	if d == nil || (req.owner == "" && req.root == "") {
		return
	}
	select {
	case <-d.ctx.Done():
		return
	default:
	}
	key := req.key()
	d.mu.Lock()
	if d.queued[key] {
		d.mu.Unlock()
		return
	}
	d.queued[key] = true
	d.mu.Unlock()
	select {
	case d.queue <- req:
	default:
		// Bounded: drop the newest overflow; the durable debt is
		// level-readable and the next eligible recheck still wakes.
		d.release(req)
	}
}

func (d *TimeoutWakeDispatcher) release(req pwBatchWakeRequest) {
	d.mu.Lock()
	delete(d.queued, req.key())
	d.mu.Unlock()
}

func (d *TimeoutWakeDispatcher) run() {
	defer close(d.done)
	var dispatches sync.WaitGroup
	// Every return path joins in-flight handlers before done closes.
	defer dispatches.Wait()
	dispatchSlots := make(chan struct{}, TimeoutWakeQueueSize)
	for {
		select {
		case <-d.ctx.Done():
			return
		case req := <-d.queue:
			select {
			case <-d.ctx.Done():
				return
			default:
			}
			d.mu.Lock()
			coord := d.coord
			if coord == nil {
				// Early completion before the coordinator attached: keep
				// the debt awake; Attach replays it. Bounded at the same
				// queue capacity, dropping the oldest.
				d.pend = append(d.pend, req)
				if len(d.pend) > TimeoutWakeQueueSize {
					drop := d.pend[0]
					d.pend = d.pend[1:]
					delete(d.queued, drop.key())
				}
				d.mu.Unlock()
				continue
			}
			d.mu.Unlock()
			owner := req.owner
			if owner == "" && req.root != "" {
				if d.rootOwner == nil {
					d.release(req)
					continue
				}
				resolved, err := d.rootOwner(d.ctx, req.root)
				if err != nil || resolved == "" {
					d.release(req)
					continue
				}
				owner = resolved
			}
			if d.skipOwner != nil && d.skipOwner(d.ctx, owner) {
				d.release(req)
				continue
			}
			select {
			case dispatchSlots <- struct{}{}:
			case <-d.ctx.Done():
				d.release(req)
				return
			}
			dispatches.Add(1)
			go func(req pwBatchWakeRequest, coord agent.Coordinator, owner string) {
				defer dispatches.Done()
				defer func() { <-dispatchSlots }()
				_ = agent.DispatchSessionWake(d.ctx, coord, owner)
				d.release(req)
			}(req, coord, owner)
		}
	}
}

// SetRootOwner wires the durable-row owner resolver (the install path uses
// it instead of touching the unexported field).
func (d *TimeoutWakeDispatcher) SetRootOwner(f func(context.Context, string) (string, error)) {
	d.mu.Lock()
	d.rootOwner = f
	d.mu.Unlock()
}

// setSkipOwner wires the owner veto predicate.
func (d *TimeoutWakeDispatcher) setSkipOwner(f func(context.Context, string) bool) {
	d.mu.Lock()
	d.skipOwner = f
	d.mu.Unlock()
}

// PendingWakeDepths reports the queued and pending-unattached wake counts
// (test observability of the dispatcher's internal buffers).
func (d *TimeoutWakeDispatcher) PendingWakeDepths() (queued, pending int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queued), len(d.pend)
}

// Coordinator returns the attached coordinator, nil before Attach (the
// getter mirrors BatchRuntime.Runner).
func (d *TimeoutWakeDispatcher) Coordinator() agent.Coordinator {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.coord
}

// Done reports the dispatcher's join channel (closed after Stop's goroutine
// join completes); the shutdown tests observe it directly.
func (d *TimeoutWakeDispatcher) Done() <-chan struct{} {
	return d.done
}

func (d *TimeoutWakeDispatcher) Stop() {
	if d == nil {
		return
	}
	d.stop.Do(func() {
		d.cancel()
		d.Start()
		<-d.done
	})
}
