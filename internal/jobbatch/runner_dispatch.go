package jobbatch

import (
	"context"
	"encoding/json"
	"errors"
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
