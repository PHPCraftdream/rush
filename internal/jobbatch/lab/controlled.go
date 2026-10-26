package lab

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

type controlledPhase uint8

const (
	controlledRegistered controlledPhase = iota
	controlledAccepted
	controlledReported
	controlledRetired
	controlledDisposed
)

type controlledObservation struct {
	Token uint64
	Kind  string
	Call  uint64
}

type ControlledExecutor struct {
	mu       sync.Mutex
	changed  chan struct{}
	attempts map[uint64]*controlledAttempt
	order    []*controlledAttempt
	// Set before use; callbacks must be nonblocking and must not reenter.
	observe func(controlledObservation)
}

type controlledGate struct {
	done     chan struct{}
	result   GateResult
	released bool
}

type controlledAttempt struct {
	owner       *ControlledExecutor
	launch      jobbatch.Launch
	report      func(jobbatch.Result)
	phase       controlledPhase
	awaited     bool
	decision    *StartDecision
	startDone   chan struct{}
	holdStop    bool
	failStop    *string
	stops       []*controlledGate
	stopActive  bool
	holdDispose bool
	dispose     *controlledGate
	holdRead    bool
	read        *controlledGate
	reads       int
	output      []byte
	messages    []json.RawMessage
}

type controlledError struct{ code string }

func (e controlledError) Error() string { return "batchlab-error-sentinel" }
func (e controlledError) Code() string  { return e.code }

func NewControlledExecutor() *ControlledExecutor {
	return &ControlledExecutor{changed: make(chan struct{}), attempts: make(map[uint64]*controlledAttempt)}
}

func (c *ControlledExecutor) signal() { close(c.changed); c.changed = make(chan struct{}) }
func (c *ControlledExecutor) observation(a *controlledAttempt, kind string, call uint64) {
	if c.observe != nil {
		c.observe(controlledObservation{Token: a.launch.Token, Kind: kind, Call: call})
	}
}

func (c *ControlledExecutor) attempt(token uint64) (*controlledAttempt, error) {
	a := c.attempts[token]
	if a == nil || a.phase == controlledRetired || a.phase == controlledDisposed {
		return nil, ErrUnknownAttempt
	}
	return a, nil
}

func controlledDecisionValid(d StartDecision) bool {
	switch d.Decision {
	case DecisionAccept, DecisionRefuseCapacity:
		return d.State == "" && d.Code == ""
	case DecisionFail:
		return d.State == ""
	case DecisionAcceptComplete:
		return d.State.Reportable()
	}
	return false
}

func (c *ControlledExecutor) Start(ctx context.Context, l jobbatch.Launch, report func(jobbatch.Result)) (jobbatch.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	if _, exists := c.attempts[l.Token]; exists {
		c.mu.Unlock()
		return nil, ErrAttemptPhase
	}
	a := &controlledAttempt{owner: c, launch: l, report: report, phase: controlledRegistered, startDone: make(chan struct{})}
	c.attempts[l.Token] = a
	c.order = append(c.order, a)
	c.observation(a, "start_entered", 0)
	c.signal()
	defer func() {
		c.mu.Lock()
		c.observation(a, "start_returned", 0)
		close(a.startDone)
		c.signal()
		c.mu.Unlock()
	}()
	for a.decision == nil && ctx.Err() == nil {
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		c.mu.Lock()
	}
	if err := ctx.Err(); err != nil {
		a.phase = controlledRetired
		c.signal()
		c.mu.Unlock()
		return nil, err
	}
	d := *a.decision
	switch d.Decision {
	case DecisionRefuseCapacity:
		a.phase = controlledRetired
		c.mu.Unlock()
		return nil, jobbatch.ErrCapacity
	case DecisionFail:
		a.phase = controlledRetired
		c.mu.Unlock()
		return nil, controlledError{d.Code}
	}
	a.phase = controlledAccepted
	if d.Decision == DecisionAcceptComplete {
		a.phase = controlledReported
		c.observation(a, "report", 0)
	}
	c.signal()
	c.mu.Unlock()
	if d.Decision == DecisionAcceptComplete {
		report(jobbatch.Result{State: d.State, Code: d.Code})
	}
	return a, nil
}

func (c *ControlledExecutor) AwaitStart(ctx context.Context, id jobbatch.ID) (jobbatch.Launch, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		for _, a := range c.order {
			if a.launch.ID == id && !a.awaited {
				a.awaited = true
				l := a.launch
				l.Task.Payload = append(json.RawMessage(nil), l.Task.Payload...)
				return l, nil
			}
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			c.mu.Lock()
			return jobbatch.Launch{}, ctx.Err()
		}
		c.mu.Lock()
	}
}

func (c *ControlledExecutor) ReleaseStart(ctx context.Context, token uint64, d StartDecision) error {
	c.mu.Lock()
	a, err := c.attempt(token)
	if err == nil && (a.phase != controlledRegistered || a.decision != nil || !controlledDecisionValid(d)) {
		err = ErrAttemptPhase
	}
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return err
	}
	a.decision = &d
	c.signal()
	c.mu.Unlock()
	select {
	case <-a.startDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *ControlledExecutor) Complete(ctx context.Context, token uint64, r jobbatch.Result) error {
	c.mu.Lock()
	a, err := c.attempt(token)
	if err == nil && a.phase == controlledReported {
		err = ErrUnknownAttempt
	}
	if err == nil && a.phase != controlledAccepted {
		err = ErrAttemptPhase
	}
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return err
	}
	a.phase = controlledReported
	r.Details = append(json.RawMessage(nil), r.Details...)
	c.observation(a, "report", 0)
	c.signal()
	c.mu.Unlock()
	a.report(r)
	return nil
}

func (c *ControlledExecutor) Emit(token uint64, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if a.phase != controlledAccepted {
		return ErrAttemptPhase
	}
	a.output = append(a.output, data...)
	c.signal()
	return nil
}

func (c *ControlledExecutor) AwaitInject(ctx context.Context, token uint64) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return nil, err
	}
	if a.phase != controlledAccepted {
		return nil, ErrAttemptPhase
	}
	for {
		if _, err := c.attempt(token); err != nil {
			return nil, err
		}
		if a.phase != controlledAccepted {
			return nil, ErrAttemptPhase
		}
		if len(a.messages) > 0 {
			msg := append(json.RawMessage(nil), a.messages[0]...)
			a.messages = a.messages[1:]
			return msg, nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			c.mu.Lock()
			return nil, ctx.Err()
		}
		c.mu.Lock()
	}
}

func (a *controlledAttempt) Inject(ctx context.Context, msg json.RawMessage) error {
	c := a.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if a.phase != controlledAccepted {
		return jobbatch.ErrExecutionDone
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.messages = append(a.messages, append(json.RawMessage(nil), msg...))
	c.signal()
	return nil
}

var (
	_ jobbatch.Executor     = (*ControlledExecutor)(nil)
	_ jobbatch.Execution    = (*controlledAttempt)(nil)
	_ jobbatch.OutputReader = (*controlledAttempt)(nil)
	_ jobbatch.Messenger    = (*controlledAttempt)(nil)
	_ jobbatch.Disposer     = (*controlledAttempt)(nil)
)
