package app

import (
	"context"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent"
)

const batchTimeoutWakeQueueSize = 32

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
	mu     sync.Mutex
	coord  agent.Coordinator
	queue  chan string
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	stop   sync.Once
	start  sync.Once
}

func newBatchTimeoutWakeDispatcher() *batchTimeoutWakeDispatcher {
	ctx, cancel := context.WithCancel(context.Background())
	return &batchTimeoutWakeDispatcher{queue: make(chan string, batchTimeoutWakeQueueSize), ctx: ctx, cancel: cancel, done: make(chan struct{})}
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
	d.mu.Unlock()
	d.Start()
}

func (d *batchTimeoutWakeDispatcher) Wake(_ context.Context, owner string) {
	if d == nil || owner == "" {
		return
	}
	select {
	case <-d.ctx.Done():
		return
	default:
	}
	select {
	case d.queue <- owner:
	default:
	}
}

func (d *batchTimeoutWakeDispatcher) run() {
	defer close(d.done)
	for {
		select {
		case <-d.ctx.Done():
			return
		case owner := <-d.queue:
			select {
			case <-d.ctx.Done():
				return
			default:
			}
			d.mu.Lock()
			coord := d.coord
			d.mu.Unlock()
			if coord != nil {
				_ = agent.DispatchSessionWake(d.ctx, coord, owner)
			}
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
