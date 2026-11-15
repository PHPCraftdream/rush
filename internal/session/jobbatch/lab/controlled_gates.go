package lab

import (
	"context"
	"strings"
)

func (c *ControlledExecutor) HoldStop(token uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if a.holdStop {
		return ErrAttemptPhase
	}
	a.holdStop = true
	return nil
}

func (c *ControlledExecutor) FailNextStop(token uint64, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if a.failStop != nil {
		return ErrAttemptPhase
	}
	a.failStop = &code
	return nil
}

func (c *ControlledExecutor) AwaitStop(ctx context.Context, token uint64, call uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if call == 0 {
		return ErrAttemptPhase
	}
	for {
		if _, err := c.attempt(token); err != nil {
			return err
		}
		if uint64(len(a.stops)) >= call {
			return nil
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			c.mu.Lock()
			return ctx.Err()
		}
		c.mu.Lock()
	}
}

func (c *ControlledExecutor) ReturnStop(token uint64, call uint64, result GateResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if call == 0 || call > uint64(len(a.stops)) || !scenarioGateValid(result) {
		return ErrAttemptPhase
	}
	return controlledReleaseGate(a.stops[call-1], result)
}

func controlledReleaseGate(g *controlledGate, result GateResult) error {
	if g == nil || g.done == nil || g.released {
		return ErrAttemptPhase
	}
	g.result, g.released = result, true
	close(g.done)
	return nil
}

func controlledGateResult(result GateResult) error {
	if result == GatePanic {
		panic("batchlab-error-sentinel")
	}
	if strings.HasPrefix(string(result), "error:") {
		return controlledError{strings.TrimPrefix(string(result), "error:")}
	}
	return nil
}

func (a *controlledAttempt) Stop(ctx context.Context) error {
	c := a.owner
	c.mu.Lock()
	if a.phase == controlledDisposed || a.phase == controlledRetired {
		c.mu.Unlock()
		return ErrUnknownAttempt
	}
	if a.stopActive {
		c.mu.Unlock()
		return ErrAttemptPhase
	}
	a.stopActive = true
	g := &controlledGate{result: GateOK}
	if a.failStop != nil {
		g.result = GateResult("error:" + *a.failStop)
		a.failStop = nil
	}
	if a.holdStop {
		g.done = make(chan struct{})
		a.holdStop = false
	}
	a.stops = append(a.stops, g)
	call := uint64(len(a.stops))
	c.observation(a, "stop_entered", call)
	c.signal()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		a.stopActive = false
		c.observation(a, "stop_returned", call)
		c.signal()
		c.mu.Unlock()
	}()
	if g.done != nil {
		<-g.done
	}
	return controlledGateResult(g.result)
}

func (c *ControlledExecutor) HoldDispose(token uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if a.phase != controlledAccepted && a.phase != controlledReported || a.holdDispose || a.dispose != nil {
		return ErrAttemptPhase
	}
	a.holdDispose = true
	return nil
}

func (c *ControlledExecutor) AwaitDispose(ctx context.Context, token uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempts[token]
	if a == nil || a.phase == controlledRetired {
		return ErrUnknownAttempt
	}
	if a.phase == controlledRegistered {
		return ErrAttemptPhase
	}
	for a.dispose == nil {
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			c.mu.Lock()
			return ctx.Err()
		}
		c.mu.Lock()
	}
	return nil
}

func (c *ControlledExecutor) ReturnDispose(token uint64, result GateResult) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if a.phase != controlledReported || !scenarioGateValid(result) {
		return ErrAttemptPhase
	}
	return controlledReleaseGate(a.dispose, result)
}

func (a *controlledAttempt) Dispose(ctx context.Context) error {
	c := a.owner
	c.mu.Lock()
	if a.phase != controlledReported || a.dispose != nil || a.stopActive || a.reads != 0 {
		c.mu.Unlock()
		return ErrAttemptPhase
	}
	g := &controlledGate{result: GateOK}
	if a.holdDispose {
		g.done = make(chan struct{})
		a.holdDispose = false
	}
	a.dispose = g
	c.observation(a, "dispose_entered", 0)
	c.signal()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		a.phase = controlledDisposed
		c.observation(a, "dispose_returned", 0)
		c.signal()
		c.mu.Unlock()
	}()
	if g.done != nil {
		<-g.done
	}
	return controlledGateResult(g.result)
}

func (c *ControlledExecutor) HoldRead(token uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if a.phase != controlledAccepted || a.holdRead {
		return ErrAttemptPhase
	}
	a.holdRead = true
	return nil
}

func (c *ControlledExecutor) ReturnRead(token uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, err := c.attempt(token)
	if err != nil {
		return err
	}
	if a.phase != controlledAccepted && a.phase != controlledReported {
		return ErrAttemptPhase
	}
	return controlledReleaseGate(a.read, GateOK)
}
