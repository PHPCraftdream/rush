package jobbatch

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
)

type runnerAttempt struct {
	ctx                    context.Context
	dispatched             bool
	root                   ID
	launch                 Launch
	cancel                 context.CancelFunc
	timer                  runnerTimer
	resultReady            bool
	result                 Result
	startReturned          bool
	accepted               bool
	retired                bool
	physicalReturn         bool
	physicalReport         bool
	contain                bool
	stopWaiting            bool
	stopCommandSequence    uint64
	stopDispatchedSequence uint64
	handle                 *runnerHandle
}

// indexAttempt records an attempt under its root so a durable Release can
// prune every attempt of the root, including rejected ones. A superseded
// rejected attempt for the same node is dropped immediately, mirroring the
// non-durable rejection path: a late message for the dropped token finds no
// attempt and is ignored. Loop-only.
func (r *Runner) indexAttempt(a *runnerAttempt) {
	if a.root == "" {
		return
	}
	if r.attemptsByRoot == nil {
		r.attemptsByRoot = make(map[ID]map[uint64]bool)
	}
	tokens := r.attemptsByRoot[a.root]
	for token := range tokens {
		old := r.attempts[token]
		if old == nil {
			// Already dropped by the non-durable rejection path, which
			// deletes the attempt but not its index entry.
			delete(tokens, token)
			continue
		}
		if old != a && old.launch.ID == a.launch.ID && old.retired {
			delete(r.attempts, token)
			delete(tokens, token)
		}
	}
	if tokens == nil {
		tokens = make(map[uint64]bool)
		r.attemptsByRoot[a.root] = tokens
	}
	tokens[a.launch.Token] = true
}

// pruneRootAttempts deletes every attempt of a released root. Loop-only.
func (r *Runner) pruneRootAttempts(root ID) {
	for token := range r.attemptsByRoot[root] {
		delete(r.attempts, token)
	}
	delete(r.attemptsByRoot, root)
}

type runnerMessageKind uint8

const (
	runnerStartReturned runnerMessageKind = iota
	runnerCompletion
	runnerDeadline
	runnerStopResult
	runnerStopReturned
	runnerClosingIntent
	runnerFaultSignal
)

type runnerMessage struct {
	kind      runnerMessageKind
	token     uint64
	execution Execution
	reason    RejectReason
	code      string
	result    Result
	control   *runnerStopCall
}

func (r *Runner) dispatchStart(l Launch) {
	a := r.attempts[l.Token]
	if a == nil {
		ctx, cancel := context.WithCancel(context.WithoutCancel(r.host))
		v, _ := r.engine.View(l.ID)
		a = &runnerAttempt{ctx: ctx, root: v.Root, launch: l, cancel: cancel}
		r.attempts[l.Token] = a
		r.indexAttempt(a)
	}
	if a.dispatched || a.retired {
		return
	}
	if r.commitHook != nil && a.ctx.Err() != nil {
		a.physicalReturn = true
		r.apply(Event{Type: EventStartRejected, Node: l.ID, Token: l.Token, Reason: RejectCancelled, Code: CodeStartFailed})
		return
	}
	a.dispatched = true
	ctx := a.ctx
	token := l.Token
	reported := &atomic.Bool{}
	if l.Task.Timeout > 0 {
		a.timer = r.clock.AfterFunc(l.Task.Timeout, func() { r.enqueue(runnerMessage{kind: runnerDeadline, token: token}) })
	}
	report := func(result Result) {
		if !reported.CompareAndSwap(false, true) {
			return
		}
		result.Details = append(json.RawMessage(nil), result.Details...)
		result.Code = runnerNorm(result.Code, "")
		if !result.State.Reportable() {
			result.State = StateFailed
			result.Code = CodeExecutorInvalidState
		}
		r.enqueue(runnerMessage{kind: runnerCompletion, token: token, result: result})
	}
	go func() {
		m := runnerMessage{kind: runnerStartReturned, token: l.Token}
		func() {
			defer func() {
				if recover() != nil {
					m.execution = nil
					m.reason = RejectFailure
					m.code = CodeStartPanic
				}
			}()
			execution, err := r.executor.Start(ctx, l, report)
			if err != nil {
				m.reason, m.code = runnerStartError(err)
				return
			}
			if execution == nil {
				m.reason = RejectFailure
				m.code = CodeStartNilExecution
				return
			}
			m.execution = execution
		}()
		r.enqueue(m)
	}()
}

// Both errors.Is (capacity classification) and errors.As/Code can execute
// executor-owned methods; contain their panics without reading any error text.
func runnerStartError(err error) (reason RejectReason, code string) {
	reason = RejectFailure
	code = CodeInvalid
	defer func() {
		if recover() != nil {
			reason = RejectFailure
			code = CodeInvalid
		}
	}()
	if errors.Is(err, ErrCapacity) {
		return RejectCapacity, ""
	}
	var coded interface{ Code() string }
	raw := ""
	if errors.As(err, &coded) {
		raw = coded.Code()
	}
	return RejectFailure, runnerNorm(raw, CodeStartFailed)
}

func runnerNorm(raw, fallback string) (code string) {
	code = CodeInvalid
	defer func() {
		if recover() != nil {
			code = CodeInvalid
		}
	}()
	if raw == "" {
		return fallback
	}
	if !ValidCode(raw) || ReservedCode(raw) {
		return CodeInvalid
	}
	return raw
}

func (r *Runner) handleMessage(m runnerMessage) {
	fault := r.FaultErr() != nil
	if m.kind == runnerFaultSignal {
		return
	}
	if m.kind == runnerClosingIntent {
		if fault || r.finalApplied {
			return
		}
		if !r.closing {
			r.closing = true
			r.apply(Event{Type: EventHostClosing})
		}
		return
	}
	a := r.attempts[m.token]
	if a == nil || (a.retired && !fault) {
		return
	}
	switch m.kind {
	case runnerStartReturned:
		if a.startReturned {
			return
		}
		a.startReturned = true
		a.physicalReturn = true
		if fault {
			if m.execution != nil {
				r.registerHandle(a, m.execution)
			}
			return
		}
		if m.execution == nil {
			reason := m.reason
			if v, ok := r.engine.View(a.launch.ID); ok && v.Cause != CauseNone && reason != RejectCapacity {
				reason = RejectCancelled
				if m.code == "" {
					m.code = CodeStartFailed
				}
			}
			r.apply(Event{Type: EventStartRejected, Node: a.launch.ID, Token: m.token, Reason: reason, Code: m.code})
			a.result = Result{}
			a.resultReady = false
			return
		}
		a.accepted = true
		r.registerHandle(a, m.execution)
		if r.requestHooks != nil && r.requestHooks.beforeStartedApply != nil {
			r.requestHooks.beforeStartedApply()
		}
		if _, err := r.apply(Event{Type: EventStarted, Node: a.launch.ID, Token: m.token}); err != nil {
			if errors.Is(err, ErrCommitFailed) && a.physicalReport {
				a.handle.terminate()
			}
			return
		}
		if a.stopWaiting {
			r.dispatchWaitingStop(a)
		}
		if a.resultReady {
			r.acceptCompletion(a)
		}
	case runnerCompletion:
		if fault {
			a.physicalReport = true
			if a.handle != nil {
				a.handle.terminate()
			}
			return
		}
		if a.resultReady {
			return
		}
		a.physicalReport = true
		a.result = m.result
		a.resultReady = true
		if a.accepted {
			r.acceptCompletion(a)
		}
	case runnerDeadline:
		if fault {
			a.cancel()
			return
		}
		r.apply(Event{Type: EventDeadlineExpired, Node: a.launch.ID, Token: m.token})
	case runnerStopResult:
		if fault {
			return
		}
		if m.control != nil && a.handle != nil {
			a.handle.mu.Lock()
			terminal := a.handle.terminated
			current := a.handle.stopCall == m.control
			a.handle.mu.Unlock()
			if !terminal && current {
				r.apply(Event{Type: EventControlReported, Node: a.launch.ID, Token: m.token, Op: ControlOpStop, Code: m.code})
			}
		}
	case runnerStopReturned:
		// The physical worker already released its pin/slot. Only the loop may
		// dispatch the queued command, after checking accepted termination state.
		h := a.handle
		if h == nil {
			return
		}
		h.mu.Lock()
		queued := h.stopQueued && !h.terminated && !h.stopActive
		if queued {
			h.stopQueued = false
		}
		h.mu.Unlock()
		if queued {
			r.startOrQueueStop(h)
		}
	}
}

func (r *Runner) acceptCompletion(a *runnerAttempt) {
	if r.requestHooks != nil && r.requestHooks.beforeSettlementApply != nil {
		r.requestHooks.beforeSettlementApply()
	}
	if _, err := r.apply(Event{Type: EventSettled, Node: a.launch.ID, Token: a.launch.Token, Result: a.result}); err == nil {
		// Details have crossed Apply's ownership boundary; retain no extra copy.
		a.result = Result{}
	} else if errors.Is(err, ErrCommitFailed) && a.handle != nil {
		// The report is a physical termination fact even when its Engine
		// transition cannot be accepted across the concurrent fault boundary.
		a.handle.terminate()
	}
}

// dropSuperseded removes a just-retired attempt when a newer attempt of the
// same node was already indexed in the same transition (the durable apply
// creates the re-authorised attempt before it retires the rejected one, so
// indexAttempt cannot see the retirement). Loop-only.
func (r *Runner) dropSuperseded(a *runnerAttempt) {
	tokens := r.attemptsByRoot[a.root]
	for token := range tokens {
		if n := r.attempts[token]; n != nil && n != a && n.launch.ID == a.launch.ID && n.launch.Token > a.launch.Token {
			delete(r.attempts, a.launch.Token)
			delete(tokens, a.launch.Token)
			return
		}
	}
}

// The retained feed has its own lock and remains immutable after Final.
type runnerFeed struct {
	mu          sync.Mutex
	retention   int
	records     []Record
	start       int
	head, final uint64
	changed     chan struct{}
	// Only the loop uses known; it distinguishes creation from changed views.
	known map[ID]bool
}

func (r *Runner) appendTransition(t Transition) {
	if r.requestHooks != nil && r.requestHooks.beforeAppend != nil {
		r.requestHooks.beforeAppend(t)
	}
	r.appendRecords(r.captureRecords(t), t.Final)
}

// captureRecords is owner-only; publication never consults the live Engine.
func (r *Runner) captureRecords(t Transition) []Record {
	e := t.Event
	rec := Record{Sequence: t.Sequence, Type: RecordType(e.Type), Root: e.Root, Node: e.Node, Token: e.Token, Reason: e.Reason, Op: e.Op, Code: e.Code, Group: e.Group, Ref: e.Ref}
	if e.Type == EventSubmit {
		rec.Node = e.Root
	}
	if e.Type == EventAdd {
		rec.Node = e.Parent
	}
	if e.Type == EventRelease {
		rec.Node = e.Root
	}
	if rec.Node != "" {
		if v, ok := r.engine.View(rec.Node); ok {
			rec.Root = v.Root
			// State and cause describe accepted state; outcome code is separately
			// preserved for Settled, including when cancellation wins the state.
			rec.State = v.State
			rec.Cause = v.Cause
			if v.Kind == KindTask {
				rec.Group = v.Group
			}
		}
	}
	if e.Type == EventSettled {
		// Replay needs the reported state, not the cancellation-mapped leaf state.
		rec.State = e.Result.State
		rec.Code = e.Result.Code
	}
	f := &r.feed
	if f.known == nil {
		f.known = make(map[ID]bool)
	}
	for _, v := range t.Changed {
		if !f.known[v.ID] {
			if e.Type == EventSubmit || e.Type == EventAdd {
				rec.IDs = append(rec.IDs, v.ID)
			}
			f.known[v.ID] = true
		}
	}
	records := []Record{rec}
	for _, cmd := range t.Commands {
		node, token := cmd.Node, cmd.Token
		if cmd.Type == CommandStartLeaf {
			node = cmd.Launch.ID
			token = cmd.Launch.Token
		}
		records = append(records, Record{Sequence: t.Sequence, Type: RecordCommand, Command: cmd.Type, Node: node, Token: token, Cause: cmd.Cause})
	}
	for _, n := range t.Notices {
		counts := n.Counts
		records = append(records, Record{Sequence: t.Sequence, Type: RecordNotice, Root: n.Root, Node: n.Node, Kind: n.Kind, State: n.State, Cause: n.Cause, Counts: &counts})
	}
	if t.Final {
		records = append(records, Record{Sequence: t.Sequence, Type: RecordClosed})
	}
	return records
}

func (r *Runner) appendRecords(records []Record, final bool) {
	f := &r.feed
	f.mu.Lock()
	for _, record := range records {
		f.head++
		record.Cursor = f.head
		if len(f.records) < f.retention {
			f.records = append(f.records, record)
		} else {
			f.records[f.start] = record
			f.start = (f.start + 1) % f.retention
		}
	}
	if final {
		f.final = f.head
	}
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

func runnerCopyRecord(rec Record) Record {
	rec.IDs = append([]ID(nil), rec.IDs...)
	if rec.Counts != nil {
		counts := *rec.Counts
		rec.Counts = &counts
	}
	return rec
}

func (r *Runner) Events(ctx context.Context, cursor uint64) (EventPage, error) {
	f := &r.feed
	for {
		f.mu.Lock()
		oldest := f.head + 1
		if len(f.records) > 0 {
			oldest = f.records[f.start].Cursor
		}
		page := EventPage{Next: cursor, Oldest: oldest, Head: f.head, Final: f.final}
		if cursor < oldest {
			page.Next = oldest
			f.mu.Unlock()
			return page, r.resultAfterFault(ErrGap)
		}
		if cursor <= f.head {
			if cursor >= oldest {
				start := int(cursor - oldest)
				for i := start; i < len(f.records); i++ {
					index := (f.start + i) % len(f.records)
					page.Records = append(page.Records, runnerCopyRecord(f.records[index]))
				}
			}
			page.Next = page.Records[len(page.Records)-1].Cursor + 1
			f.mu.Unlock()
			return page, nil
		}
		if f.final != 0 {
			page.Next = f.final + 1
			f.mu.Unlock()
			return page, r.resultAfterFault(ErrClosed)
		}
		if err := r.FaultErr(); err != nil {
			page.Next = f.head + 1
			f.mu.Unlock()
			return page, err
		}
		changed := f.changed
		f.mu.Unlock()
		if r.requestHooks != nil && r.requestHooks.beforeEventsContext != nil {
			r.requestHooks.beforeEventsContext()
		}
		// Future cursors wait for producer progress as well; no polling or skipping.
		if err := ctx.Err(); err != nil {
			if r.requestHooks != nil && r.requestHooks.beforeEventsFault != nil {
				r.requestHooks.beforeEventsFault()
			}
			if fault := r.FaultErr(); fault != nil {
				f.mu.Lock()
				progress := cursor <= f.head
				f.mu.Unlock()
				if progress {
					continue
				}
				return page, fault
			}
			return page, err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			if r.requestHooks != nil && r.requestHooks.beforeEventsFault != nil {
				r.requestHooks.beforeEventsFault()
			}
			if fault := r.FaultErr(); fault != nil {
				f.mu.Lock()
				progress := cursor <= f.head
				f.mu.Unlock()
				if progress {
					continue
				}
				return page, fault
			}
			return page, ctx.Err()
		}
	}
}

// testBatchOnce tracks the settlement gate of every Batch created by
// NewTestBatch, so Settle closes Done exactly once (a real Batch closes its
// done channel exactly once when the run settles — runner.go publishes
// SettledRoots with close(b.done)).
var testBatchOnce sync.Map // *Batch -> *sync.Once

// NewTestBatch builds a detached Batch handle for cross-package tests (e.g.
// agent ingress runner fakes that must return (root, *Batch) from
// SubmitBound). The handle reports the given id and mimics the real Batch's
// Done semantics: the channel is open while the batch runs and closes exactly
// once when Settle is called. It is wired to no Runner and never settles on
// its own.
//
// There is deliberately no production constructor equivalent: a real Batch
// only ever comes from Runner.Submit, and tests cannot reach the unexported
// fields from outside this package.
func NewTestBatch(id ID) *Batch {
	b := &Batch{id: id, done: make(chan struct{})}
	testBatchOnce.Store(b, &sync.Once{})
	return b
}

// Settle closes the test batch's Done channel exactly once (sync.Once),
// mimicking a real Batch's close-on-completion: every watcher — including the
// agent root-lease adoption watcher — observes settlement and releases.
// Settle is IDEMPOTENT: the map entry is retained for the process lifetime
// (the Once makes every call after the first a no-op), so a second Settle on
// the same Batch is harmless. Test-only API. It panics on a Batch not
// produced by NewTestBatch.
func (b *Batch) Settle() {
	v, ok := testBatchOnce.Load(b)
	if !ok {
		panic("jobbatch: Settle called on a non-test Batch")
	}
	once := v.(*sync.Once)
	once.Do(func() {
		close(b.done)
	})
}
