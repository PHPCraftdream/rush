package jobbatch

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
)

// runnerHandle's lock protects physical lifetime, independently of Engine state.
// The loop alone authorizes new calls; actual returns alone release pins.
type runnerHandle struct {
	mu              sync.Mutex
	runner          *Runner
	node, root      ID
	token           uint64
	execution       Execution
	pins            int
	terminated      bool
	givenUp         bool
	stopActive      bool
	stopQueued      bool
	stopCall        *runnerStopCall
	disposing       bool
	disposeDone     bool
	disposeCode     string
	disposeRequired bool
}
type runnerStopCall struct{ once sync.Once }

func (r *Runner) dispatchStopCommand(cmd Command) {
	a := r.attempts[cmd.Token]
	if a == nil || a.retired {
		return
	}
	a.cancel()
	if a.handle == nil {
		a.stopWaiting = true
		return
	}
	r.startOrQueueStop(a.handle)
}

func (r *Runner) startOrQueueStop(h *runnerHandle) {
	h.mu.Lock()
	if h.terminated || h.givenUp {
		h.mu.Unlock()
		return
	}
	if h.stopActive {
		h.stopQueued = true
		h.mu.Unlock()
		return
	}
	call := &runnerStopCall{}
	h.stopCall = call
	h.stopQueued = false
	h.stopActive = true
	h.pins++
	h.mu.Unlock()
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.host))
	answer := func(code string) {
		call.once.Do(func() {
			r.enqueue(runnerMessage{kind: runnerStopResult, token: h.token, code: code, control: call})
		})
	}
	// §3.1.2: claim timeout before cancellation can produce ctx.Err.
	timer := r.clock.AfterFunc(r.controlTimeout, func() { answer(CodeStopTimeout); r.cancelControlTimeout(cancel) })
	go func() {
		code := ""
		func() {
			defer func() {
				if recover() != nil {
					code = CodeStopPanic
				}
			}()
			if h.execution.Stop(ctx) != nil {
				code = CodeStopFailed
			}
		}()
		answer(code)
		timer.Stop()
		cancel()
		h.mu.Lock()
		h.stopActive = false
		h.pins--
		h.mu.Unlock()
		h.maybeDispose()
		r.enqueue(runnerMessage{kind: runnerStopReturned, token: h.token})
		r.controlReturned()
	}()
}

func (h *runnerHandle) terminate() {
	h.mu.Lock()
	h.terminated = true
	h.stopQueued = false
	h.mu.Unlock()
	h.maybeDispose()
}

func (h *runnerHandle) giveUp() {
	h.mu.Lock()
	h.givenUp = true
	h.stopQueued = false
	h.mu.Unlock()
	h.maybeDispose()
}

func (h *runnerHandle) unpin() {
	h.mu.Lock()
	h.pins--
	h.mu.Unlock()
	h.maybeDispose()
}

func (h *runnerHandle) maybeDispose() {
	h.mu.Lock()
	if !h.terminated || !h.givenUp || h.pins != 0 || h.disposing || h.disposeDone {
		h.mu.Unlock()
		return
	}
	disposer, ok := h.execution.(Disposer)
	if !ok {
		h.disposeDone = true
		h.execution = nil
		h.mu.Unlock()
		h.runner.cleanupNotify()
		return
	}
	h.disposing = true
	h.mu.Unlock()
	r := h.runner
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.host))
	var answered atomic.Bool
	answer := func(code string) {
		if answered.CompareAndSwap(false, true) {
			h.mu.Lock()
			h.disposeDone = true
			h.disposeCode = code
			// §4.5: the physical Dispose goroutine owns its local capability;
			// a logical timeout need not retain it in Runtime metadata.
			h.execution = nil
			h.mu.Unlock()
			r.cleanupNotify()
		}
	}
	// §§3.4/4.5: timeout is the logical result, cancellation is its effect.
	timer := r.clock.AfterFunc(r.controlTimeout, func() { answer(CodeDisposeTimeout); r.cancelControlTimeout(cancel) })
	go func() {
		code := ""
		func() {
			defer func() {
				if recover() != nil {
					code = CodeDisposePanic
				}
			}()
			if disposer.Dispose(ctx) != nil {
				code = CodeDisposeFailed
			}
		}()
		answer(code)
		timer.Stop()
		cancel()
		r.controlReturned()
	}()
}

func (r *Runner) cleanupNotify() {
	r.mu.Lock()
	close(r.cleanupChanged)
	r.cleanupChanged = make(chan struct{})
	r.mu.Unlock()
}

func (r *Runner) cleanupStatus() ([]DisposeFailure, bool, <-chan struct{}) {
	r.mu.Lock()
	handles := append([]*runnerHandle(nil), r.handles...)
	changed := r.cleanupChanged
	r.mu.Unlock()
	var failures []DisposeFailure
	pending := false
	for _, h := range handles {
		h.mu.Lock()
		if h.disposeRequired {
			code := h.disposeCode
			if !h.disposeDone {
				code = CodeDisposePending
				pending = true
			}
			if code != "" {
				failures = append(failures, DisposeFailure{Node: h.node, Token: h.token, Code: code})
			}
		}
		h.mu.Unlock()
	}
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].Node == failures[j].Node {
			return failures[i].Token < failures[j].Token
		}
		return failures[i].Node < failures[j].Node
	})
	return failures, pending, changed
}
