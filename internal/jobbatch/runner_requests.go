package jobbatch

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// OneItem produces an ordinary batch and detaches the task's mutable payload.
func OneItem(task TaskSpec) Spec {
	task.Payload = append(json.RawMessage(nil), task.Payload...)
	return Spec{Batch: &BatchSpec{Mode: ModeParallel, Notify: NotifyAll, Items: []Spec{{Task: &task}}}}
}

func (r *Runner) Submit(ctx context.Context, req SubmitRequest) (*Batch, error) {
	if r.isClosed() {
		return nil, ErrClosed
	}
	if req.Spec.Batch == nil || req.Spec.Task != nil {
		return nil, ErrInvalidSpec
	}
	items, err := r.cloneSpecs([]Spec{req.Spec})
	if err != nil {
		return nil, err
	}
	a := r.request(ctx, &runnerRequest{event: Event{Type: EventSubmit, Root: req.Root, Spec: items[0], Ref: req.Ref}})
	return a.batch, a.err
}

func (r *Runner) Add(ctx context.Context, req AddRequest) ([]ID, error) {
	if r.isClosed() {
		return nil, ErrClosed
	}
	items, err := r.cloneSpecs(req.Items)
	if err != nil {
		return nil, err
	}
	a := r.request(ctx, &runnerRequest{event: Event{Type: EventAdd, Parent: req.Parent, Items: items, Ref: req.Ref}})
	return a.ids, a.err
}

func (r *Runner) Stop(ctx context.Context, id ID) error {
	return r.request(ctx, &runnerRequest{event: Event{Type: EventStopRequested, Node: id}}).err
}

func (r *Runner) CapacityAvailable(ctx context.Context, group string) error {
	return r.request(ctx, &runnerRequest{event: Event{Type: EventCapacityAvailable, Group: group}}).err
}

func (r *Runner) Release(ctx context.Context, id ID) error {
	return r.request(ctx, &runnerRequest{event: Event{Type: EventRelease, Root: id}}).err
}

func (r *Runner) View(ctx context.Context, id ID) (NodeView, error) {
	a := r.request(ctx, &runnerRequest{op: runnerRequestView, node: id})
	return a.view, a.err
}

func (r *Runner) Snapshot(ctx context.Context, id ID) (BatchView, error) {
	a := r.request(ctx, &runnerRequest{op: runnerRequestSnapshot, node: id})
	return a.snapshot, a.err
}

func (r *Runner) Output(ctx context.Context, id ID, cursor uint64) (chunk OutputChunk, err error) {
	a := r.request(ctx, &runnerRequest{op: runnerRequestOutput, node: id})
	if a.err != nil {
		return OutputChunk{}, a.err
	}
	defer a.handle.unpin()
	defer func() {
		if recover() != nil {
			chunk = OutputChunk{}
			err = ErrExecutorPanic
		}
	}()
	return a.handle.execution.(OutputReader).ReadOutput(ctx, cursor)
}

func (r *Runner) Inject(ctx context.Context, id ID, msg json.RawMessage) (err error) {
	msg = append(json.RawMessage(nil), msg...)
	a := r.request(ctx, &runnerRequest{op: runnerRequestInject, node: id})
	if a.err != nil {
		return a.err
	}
	defer a.handle.unpin()
	defer func() {
		if recover() != nil {
			err = ErrExecutorPanic
		}
	}()
	return a.handle.execution.(Messenger).Inject(ctx, msg)
}

func (r *Runner) handleRequest(q *runnerRequest) runnerAnswer {
	if q.op == runnerRequestClose {
		if !r.closing {
			r.closing = true
			_, err := r.apply(Event{Type: EventHostClosing})
			return runnerAnswer{err: err}
		}
		return runnerAnswer{}
	}
	if r.closing && (q.event.Type == EventSubmit || q.event.Type == EventAdd || q.op == runnerRequestInject) {
		return runnerAnswer{err: ErrClosing}
	}
	if q.op != runnerRequestEvent {
		return r.inspectRequest(q)
	}
	e := q.event
	if e.Type == EventStopRequested {
		v, err := r.lookup(e.Node)
		if err != nil {
			return runnerAnswer{err: err}
		}
		if v.State.Terminal() {
			return runnerAnswer{}
		}
	}
	var batch *Batch
	if e.Type == EventSubmit {
		// Install before Apply, since an empty root can settle in its creating event.
		if _, exists := r.batches[e.Root]; exists {
			return runnerAnswer{err: ErrDuplicateRoot}
		}
		batch = &Batch{id: e.Root, done: make(chan struct{})}
		r.batches[e.Root] = batch
	}
	// New direct children have append-only ordinals beyond the prior maximum.
	lastOrdinal := 0
	if e.Type == EventAdd {
		if s, ok := r.engine.Snapshot(e.Parent); ok {
			for _, v := range s.Descendants {
				if v.Parent == e.Parent && v.Ordinal > lastOrdinal {
					lastOrdinal = v.Ordinal
				}
			}
		}
	}
	t, err := r.apply(e)
	if err != nil {
		if batch != nil {
			delete(r.batches, e.Root)
		}
		return runnerAnswer{err: err}
	}
	answer := runnerAnswer{batch: batch}
	if e.Type == EventAdd {
		var direct []NodeView
		seen := make(map[ID]bool)
		for _, v := range t.Changed {
			if v.Ordinal > lastOrdinal && v.Parent == e.Parent && !seen[v.ID] {
				seen[v.ID] = true
				direct = append(direct, v)
			}
		}
		sort.Slice(direct, func(i, j int) bool { return direct[i].Ordinal < direct[j].Ordinal })
		for _, v := range direct {
			answer.ids = append(answer.ids, v.ID)
		}
	}
	return answer
}

func (r *Runner) releasedNode(id ID) bool {
	// Design §§3.4/4.5: Core may discard released views; only root
	// tombstones survive, including for descendant accesses.
	for root := range r.released {
		if id == root || strings.HasPrefix(string(id), string(root)+".") {
			return true
		}
	}
	return false
}

func (r *Runner) lookup(id ID) (NodeView, error) {
	if r.releasedNode(id) {
		return NodeView{}, ErrReleased
	}
	v, ok := r.engine.View(id)
	if !ok {
		return NodeView{}, ErrUnknownNode
	}
	if r.released[v.Root] {
		return NodeView{}, ErrReleased
	}
	return v, nil
}

func (r *Runner) inspectRequest(q *runnerRequest) runnerAnswer {
	v, err := r.lookup(q.node)
	if err != nil {
		return runnerAnswer{err: err}
	}
	switch q.op {
	case runnerRequestView:
		return runnerAnswer{view: v}
	case runnerRequestSnapshot:
		if v.Kind != KindBatch {
			return runnerAnswer{err: ErrUnsupported}
		}
		s, ok := r.engine.Snapshot(q.node)
		if !ok {
			return runnerAnswer{err: ErrUnknownNode}
		}
		return runnerAnswer{snapshot: runnerCopySnapshot(s)}
	}
	if v.Kind != KindTask {
		return runnerAnswer{err: ErrUnsupported}
	}
	if q.op == runnerRequestInject && (v.State.Terminal() || v.State == StateCancelling) {
		return runnerAnswer{err: ErrTerminal}
	}
	a := r.attempts[v.Token]
	if a == nil || a.handle == nil {
		return runnerAnswer{err: ErrNotStarted}
	}
	h := a.handle
	if q.op == runnerRequestOutput {
		if _, ok := h.execution.(OutputReader); !ok {
			return runnerAnswer{err: ErrUnsupported}
		}
	} else {
		if _, ok := h.execution.(Messenger); !ok {
			return runnerAnswer{err: ErrUnsupported}
		}
	}
	h.mu.Lock()
	if h.givenUp {
		h.mu.Unlock()
		return runnerAnswer{err: ErrReleased}
	}
	h.pins++
	h.mu.Unlock()
	return runnerAnswer{handle: h}
}

func runnerCopySnapshot(s BatchView) BatchView {
	s.Descendants = append([]NodeView(nil), s.Descendants...)
	return s
}

// cloneSpecs is iterative, path-cycle aware and bounded before allocating child
// arrays. Shared acyclic subtrees are copied independently. Validation of policy
// and Add's root-relative depth/node budget still belongs to Engine.Apply.
func (r *Runner) cloneSpecs(src []Spec) ([]Spec, error) {
	if len(src) > r.maxNodes {
		return nil, ErrInvalidSpec
	}
	dst := make([]Spec, len(src))
	type frame struct {
		src   Spec
		dst   *Spec
		depth int
		leave *BatchSpec
	}
	stack := make([]frame, 0, len(src))
	for i := len(src) - 1; i >= 0; i-- {
		stack = append(stack, frame{src: src[i], dst: &dst[i], depth: 1})
	}
	path := make(map[*BatchSpec]bool)
	nodes := 0
	scheduled := len(src)
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if f.leave != nil {
			delete(path, f.leave)
			continue
		}
		nodes++
		if nodes > r.maxNodes || f.depth > r.maxDepth || (f.src.Task == nil) == (f.src.Batch == nil) {
			return nil, ErrInvalidSpec
		}
		if f.src.Task != nil {
			task := *f.src.Task
			task.Payload = append(json.RawMessage(nil), task.Payload...)
			f.dst.Task = &task
			continue
		}
		b := f.src.Batch
		if path[b] || len(b.Items) > r.maxNodes-scheduled {
			return nil, ErrInvalidSpec
		}
		scheduled += len(b.Items)
		copyBatch := *b
		copyBatch.Items = make([]Spec, len(b.Items))
		f.dst.Batch = &copyBatch
		path[b] = true
		stack = append(stack, frame{leave: b})
		for i := len(b.Items) - 1; i >= 0; i-- {
			stack = append(stack, frame{src: b.Items[i], dst: &copyBatch.Items[i], depth: f.depth + 1})
		}
	}
	return dst, nil
}

func (r *Runner) Close(ctx context.Context) error {
	// Close's context bounds only this waiter; publish lifetime intent even
	// when it is already cancelled, without an abandonable request.
	r.closeOnce.Do(func() { r.enqueue(runnerMessage{kind: runnerClosingIntent}) })
	select {
	case <-r.final:
	case <-ctx.Done():
		select {
		case <-r.final:
		default:
			// Final is the public boundary, not the later implicit give-up.
			// Hold the feed lock so timeout cannot race Final publication.
			r.feed.mu.Lock()
			if r.feed.final == 0 {
				err := r.closeTimeout()
				r.feed.mu.Unlock()
				return err
			}
			r.feed.mu.Unlock()
		}
	}
	for {
		failures, pending, changed := r.cleanupStatus()
		if !pending {
			if len(failures) > 0 {
				return &DisposeError{Failures: failures}
			}
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			failures, _, _ = r.cleanupStatus()
			if len(failures) == 0 {
				return nil
			}
			return &DisposeError{Failures: failures}
		}
	}
}

func (r *Runner) closeTimeout() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var nodes []ID
	for id, v := range r.views {
		if !v.State.Terminal() {
			nodes = append(nodes, id)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i] < nodes[j] })
	return &CloseTimeoutError{Nodes: nodes}
}
