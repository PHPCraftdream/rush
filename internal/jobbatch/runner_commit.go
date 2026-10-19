package jobbatch

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

const runnerPublicationLimit = 1024

type runnerPublication struct {
	transition Transition
	records    []Record
	snapshots  map[ID]BatchView
	batches    map[ID]*Batch
	request    *runnerRequest
	answer     runnerAnswer
	fenced     []fencedReply
}

// fencedReply is a recordless answer held behind an earlier queued item.
type fencedReply struct {
	q      *runnerRequest
	answer runnerAnswer
}

type runnerCommitResult struct {
	item *runnerPublication
	err  string
}

func (r *Runner) Faulted() <-chan struct{} { return r.faulted }
func (r *Runner) FaultErr() error          { r.mu.Lock(); defer r.mu.Unlock(); return r.faultErr }

// resultAfterFault arbitrates uncommitted public results against a recorded
// fatal outcome. Published summaries, retained records and written reply slots
// are checked by their callers before using this helper.
func (r *Runner) resultAfterFault(err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.faultErr != nil {
		return r.faultErr
	}
	return err
}

// The committer owns no Engine or execution bookkeeping. It returns ordered
// acknowledgements to the owner, which alone publishes and dispatches effects.
func (r *Runner) committer() {
	for {
		select {
		case <-r.exited:
			return
		case <-r.faulted:
			return
		case item := <-r.commitInput:
			if r.FaultErr() != nil {
				return
			}
			reason := ""
			if item.transition.Sequence != 0 {
				reason = r.commit(item.transition)
			}
			r.commitResults <- runnerCommitResult{item: item, err: reason}
			if reason != "" {
				return
			}
		}
	}
}

func (r *Runner) commit(t Transition) string {
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.host))
	defer cancel()
	result := make(chan string, 1)
	var gate sync.Mutex
	claimed := false
	claim := func(reason string) {
		gate.Lock()
		if claimed {
			gate.Unlock()
			return
		}
		claimed = true
		if reason != "" {
			r.signalFault(reason)
		}
		result <- reason
		gate.Unlock()
	}
	timer := r.clock.AfterFunc(r.commitTimeout, func() { claim("timeout") })
	defer timer.Stop()
	go func() {
		reason := "error"
		defer func() {
			if recover() != nil {
				reason = "panic"
			}
			claim(reason)
			if r.requestHooks != nil && r.requestHooks.commitReturned != nil {
				r.requestHooks.commitReturned()
			}
		}()
		// Hook entry and every fault signal share r.mu; the local gate also
		// excludes a watchdog claim before it has signalled the global fault.
		gate.Lock()
		r.mu.Lock()
		// This is the logical hook-start point. A later fault may overlap
		// an already-started hook, but cannot authorize another start.
		allowed := !claimed && r.faultErr == nil
		r.mu.Unlock()
		gate.Unlock()
		if !allowed {
			return
		}
		if r.commitHook(ctx, t) == nil {
			reason = ""
		}
	}()
	return <-result
}

// applyDurable captures a publication at acceptance, before later Engine inputs.
func (r *Runner) applyDurable(t Transition) (Transition, error) {
	item := &runnerPublication{transition: t, records: r.captureRecords(t), snapshots: make(map[ID]BatchView), batches: make(map[ID]*Batch)}
	for id := range r.feed.known {
		if v, ok := r.engine.View(id); ok && v.State.Terminal() {
			frozen, stored := r.frozenSnapshots[id]
			if stored {
				if frozen.Node.ID != "" {
					frozen.Sequence = t.Sequence
					item.snapshots[id] = frozen
				}
				continue
			}
			s, ok := r.engine.Snapshot(id)
			if !ok {
				// Task ids have no batch snapshot; cache the miss so later
				// transitions stop calling Snapshot for them.
				r.frozenSnapshots[id] = BatchView{}
				continue
			}
			frozen = runnerCopySnapshot(s)
			r.frozenSnapshots[id] = frozen
			frozen.Sequence = t.Sequence
			item.snapshots[id] = frozen
			continue
		}
		if snapshot, ok := r.engine.Snapshot(id); ok {
			item.snapshots[id] = runnerCopySnapshot(snapshot)
		}
	}
	for _, s := range t.SettledRoots {
		item.batches[s.Root] = r.batches[s.Root]
	}
	for _, cmd := range t.Commands {
		if cmd.Type == CommandStartLeaf {
			ctx, cancel := context.WithCancel(context.WithoutCancel(r.host))
			v, _ := r.engine.View(cmd.Launch.ID)
			a := &runnerAttempt{ctx: ctx, root: v.Root, launch: cmd.Launch, cancel: cancel}
			r.attempts[cmd.Launch.Token] = a
			r.indexAttempt(a)
		}
	}
	for _, cmd := range t.Commands {
		if cmd.Type == CommandStopLeaf {
			r.dispatchStopCommand(cmd, t.Sequence)
		}
	}
	e := t.Event
	if e.Type == EventSettled || e.Type == EventStartRejected {
		if a := r.attempts[e.Token]; a != nil {
			if a.timer != nil {
				a.timer.Stop()
				a.timer = nil
			}
			a.cancel()
			a.retired = true
			// Mirrors the non-durable retirement at runner.go:357; the attempt
			// stays indexed for late physical returns, but its Launch payload
			// must not stay reachable.
			a.launch.Task.Payload = nil
			if a.handle != nil {
				a.handle.terminate()
			}
			r.dropSuperseded(a)
		}
	}
	if e.Type == EventRelease {
		r.speculativeReleased[e.Root] = true
		r.mu.Lock()
		handles := append([]*runnerHandle(nil), r.handles...)
		r.mu.Unlock()
		for _, h := range handles {
			if h.root == e.Root {
				h.giveUp()
			}
		}
		delete(r.batches, e.Root)
	}
	if t.Final {
		r.finalApplied = true
		// Final snapshots include roots settled by earlier transitions.
		for id := range r.feed.known {
			if snapshot, ok := r.engine.Snapshot(id); ok {
				item.snapshots[id] = runnerCopySnapshot(snapshot)
			}
		}
	}
	r.publicationQueue = append(r.publicationQueue, item)
	r.currentPublication = item
	if r.requestHooks != nil && r.requestHooks.transitioned != nil {
		r.requestHooks.transitioned(t)
	}
	return t, nil
}

func (r *Runner) acceptRequest(q *runnerRequest) {
	if err := r.FaultErr(); err != nil {
		r.reply(q, runnerAnswer{err: err})
		return
	}
	if r.requestHooks != nil && r.requestHooks.queued != nil {
		r.requestHooks.queued(q)
	}
	if !q.state.CompareAndSwap(runnerQueued, runnerAccepted) {
		return
	}
	if r.requestHooks != nil && r.requestHooks.accepted != nil {
		r.requestHooks.accepted(q)
	}
	r.currentPublication = nil
	answer := r.handleRequest(q)
	if r.commitHook == nil {
		r.reply(q, answer)
		return
	}
	item := r.currentPublication
	if item == nil {
		if len(r.publicationQueue) == 0 {
			// Nothing earlier is pending, so there is no fence to sit
			// behind: reply immediately without allocating anything.
			r.reply(q, answer)
			return
		}
		// A recordless answer must not allocate a publication, but it stays
		// fenced behind the newest queued item: publish or failCommit
		// answers it right after that item's own outcome, preserving reply
		// order without growing the bounded queue. The list is bounded by
		// concurrently blocked callers, each holding its own request.
		tail := r.publicationQueue[len(r.publicationQueue)-1]
		tail.fenced = append(tail.fenced, fencedReply{q: q, answer: answer})
		return
	}
	item.request, item.answer = q, answer
	r.currentPublication = nil
}

func (r *Runner) reply(q *runnerRequest, answer runnerAnswer) {
	if r.requestHooks != nil && r.requestHooks.reply != nil {
		r.requestHooks.reply(q)
	}
	select {
	case q.reply <- answer:
	default:
	}
	if r.requestHooks != nil && r.requestHooks.committed != nil {
		r.requestHooks.committed(q)
	}
}

func (r *Runner) publishedRead(q *runnerRequest) runnerAnswer {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.releasedNode(q.node) {
		return runnerAnswer{err: ErrReleased}
	}
	v, ok := r.views[q.node]
	if !ok {
		return runnerAnswer{err: ErrUnknownNode}
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

func (r *Runner) publish(item *runnerPublication) {
	t := item.transition
	if t.Sequence != 0 {
		r.mu.Lock()
		for _, v := range t.Changed {
			r.views[v.ID] = v
		}
		for id, snapshot := range item.snapshots {
			r.snapshots[id] = snapshot
		}
		if t.Event.Type == EventRelease {
			r.released[t.Event.Root] = true
			r.pruneRootAttempts(t.Event.Root)
			for id, v := range r.views {
				if v.Root == t.Event.Root {
					delete(r.views, id)
					delete(r.snapshots, id)
					delete(r.feed.known, id)
					delete(r.frozenSnapshots, id)
				}
			}
			delete(r.speculativeReleased, t.Event.Root)
		}
		r.mu.Unlock()
		if r.requestHooks != nil && r.requestHooks.beforeAppend != nil {
			r.requestHooks.beforeAppend(t)
		}
		r.appendRecords(item.records, t.Final)
		for _, s := range t.SettledRoots {
			if b := item.batches[s.Root]; b != nil {
				b.summary = s
				close(b.done)
				if r.requestHooks != nil && r.requestHooks.donePublished != nil {
					r.requestHooks.donePublished(b, t)
				}
			}
		}

		if t.Final {
			r.mu.Lock()
			r.closed = true
			r.mu.Unlock()
		}
	}
	if item.request != nil {
		r.reply(item.request, item.answer)
	}
	for _, fr := range item.fenced {
		r.reply(fr.q, fr.answer)
	}
	item.fenced = nil
	for _, cmd := range t.Commands {
		if cmd.Type == CommandStartLeaf {
			r.dispatchStart(cmd.Launch)
		}
	}
}

func (r *Runner) signalFault(reason string) {
	r.mu.Lock()
	if r.faultErr != nil {
		r.mu.Unlock()
		return
	}
	r.faultErr = fmt.Errorf("%w: %s", ErrCommitFailed, reason)
	close(r.faulted)
	r.mu.Unlock()
	r.feed.mu.Lock()
	close(r.feed.changed)
	r.feed.changed = make(chan struct{})
	r.feed.mu.Unlock()
	r.enqueue(runnerMessage{kind: runnerFaultSignal})
}

func (r *Runner) failCommit(reason string) {
	r.signalFault(reason)
	r.containNode("")
	for _, item := range r.publicationQueue {
		if item.request != nil {
			if item.answer.handle != nil {
				item.answer.handle.unpin()
			}
			r.reply(item.request, runnerAnswer{err: r.FaultErr()})
		}
		for _, fr := range item.fenced {
			// The fenced caller never receives a handle answer, so the
			// deferred unpin its wrapper would install never runs: release
			// the pin here, mirroring the item.answer handling above.
			if fr.answer.handle != nil {
				fr.answer.handle.unpin()
			}
			r.reply(fr.q, runnerAnswer{err: r.FaultErr()})
		}
	}
	r.publicationQueue = nil
	r.mu.Lock()
	handles := append([]*runnerHandle(nil), r.handles...)
	r.mu.Unlock()
	for _, h := range handles {
		h.giveUp()
	}
}

func (r *Runner) containNode(id ID) {
	for _, a := range r.attempts {
		if a.retired {
			continue
		}
		node := a.launch.ID
		if id != "" && node != id && !strings.HasPrefix(string(node), string(id)+".") {
			continue
		}
		a.contain = true
		a.cancel()
		if !a.dispatched {
			a.physicalReturn = true
			a.retired = true
		}
		if a.handle != nil {
			r.containHandle(a.handle)
		}
	}
}

func (r *Runner) containHandle(h *runnerHandle) {
	h.mu.Lock()
	stopped := h.stopCall != nil || h.terminated
	h.mu.Unlock()
	if !stopped {
		r.startOrQueueStop(h)
	}
}

func (r *Runner) registerHandle(a *runnerAttempt, execution Execution) {
	if a.handle != nil {
		return
	}
	h := &runnerHandle{runner: r, node: a.launch.ID, root: a.root, token: a.launch.Token, execution: execution}
	_, h.disposeRequired = execution.(Disposer)
	a.handle = h
	r.mu.Lock()
	r.handles = append(r.handles, h)
	r.mu.Unlock()
	if r.commitHook != nil && a.stopWaiting {
		r.dispatchWaitingStop(a)
	}
	if a.contain {
		r.containHandle(h)
	}
	if a.physicalReport && r.commitHook != nil {
		h.terminate()
	}
	// A single fault decision owns both containment and give-up. A fault
	// arriving later is handled by the owner before its next cleanup turn.
	if r.FaultErr() != nil {
		a.contain = true
		a.cancel()
		r.containHandle(h)
		h.giveUp()
	}
}

func (r *Runner) faultDrained() (bool, <-chan struct{}) {
	pending := false
	for _, a := range r.attempts {
		if a.timer != nil {
			a.timer.Stop()
			a.timer = nil
		}
		if !a.retired && (!a.physicalReturn || (a.handle != nil && !a.physicalReport)) {
			pending = true
		}
	}
	r.mu.Lock()
	handles := append([]*runnerHandle(nil), r.handles...)
	r.mu.Unlock()
	for _, h := range handles {
		h.giveUp()
	}
	_, cleanupPending, changed := r.cleanupStatus()
	return !pending && !cleanupPending, changed
}

func (r *Runner) closeFault(ctx context.Context) error {
	select {
	case <-r.exited:
	case <-ctx.Done():
	}
	return r.FaultErr()
}
