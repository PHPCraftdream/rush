package app

import (
	"context"
	"errors"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
)

const batchTimeoutWakeQueueSize = 32

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

// pwBatchCompletionRootOwner resolves the owning session of a committed
// batch root from the durable row (batch_nodes.owner_session_id), never
// from hook or host identity. The writer is read at call time so a resolver
// created before the App's writer is bound still uses the live pool.
func pwBatchCompletionRootOwner(a *App) func(context.Context, string) (string, error) {
	return func(ctx context.Context, root string) (string, error) {
		if a == nil {
			return "", errors.New("batch completion wake: nil app")
		}
		w := a.batchCutoverWriter()
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
	dispatch *batchTimeoutWakeDispatcher
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

type batchTimeoutWakeDispatcher struct {
	mu        sync.Mutex
	coord     agent.Coordinator
	queue     chan pwBatchWakeRequest
	rootOwner func(context.Context, string) (string, error)
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

func newBatchTimeoutWakeDispatcher() *batchTimeoutWakeDispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &batchTimeoutWakeDispatcher{
		queue:  make(chan pwBatchWakeRequest, batchTimeoutWakeQueueSize),
		queued: make(map[string]bool),
		ctx:    ctx, cancel: cancel, done: make(chan struct{}),
	}
}

func (d *batchTimeoutWakeDispatcher) Start() {
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

func (d *batchTimeoutWakeDispatcher) Attach(c agent.Coordinator) {
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
func (d *batchTimeoutWakeDispatcher) Wake(_ context.Context, owner string) {
	d.enqueue(pwBatchWakeRequest{owner: owner})
}

// WakeCompletion queues the ordinary-completion wake for one committed
// root; the owner is resolved from the durable row at dispatch time. It
// never blocks and never fails its caller: the commit stack only enqueues.
func (d *batchTimeoutWakeDispatcher) WakeCompletion(_ context.Context, root string) {
	d.enqueue(pwBatchWakeRequest{root: root})
}

func (d *batchTimeoutWakeDispatcher) enqueue(req pwBatchWakeRequest) {
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

func (d *batchTimeoutWakeDispatcher) release(req pwBatchWakeRequest) {
	d.mu.Lock()
	delete(d.queued, req.key())
	d.mu.Unlock()
}

func (d *batchTimeoutWakeDispatcher) run() {
	defer close(d.done)
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
				if len(d.pend) > batchTimeoutWakeQueueSize {
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
			_ = agent.DispatchSessionWake(d.ctx, coord, owner)
			d.release(req)
		}
	}
}

func (d *batchTimeoutWakeDispatcher) Stop() {
	if d == nil {
		return
	}
	d.stop.Do(func() {
		d.cancel()
		d.Start()
		<-d.done
	})
}
