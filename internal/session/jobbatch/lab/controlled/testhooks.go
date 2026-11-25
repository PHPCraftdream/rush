package controlled

import (
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// TestToken returns the first registered token for focused integration tests.
func (c *ControlledExecutor) TestToken() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.order) == 0 {
		return 0
	}
	return c.order[0].launch.Token
}

// TestLaunch returns a copy of the first registered launch for integration assertions.
func (c *ControlledExecutor) TestLaunch() jobbatch.Launch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.order) == 0 {
		return jobbatch.Launch{}
	}
	return c.order[0].launch
}

func (c *ControlledExecutor) TestDecisionCode(token uint64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.attempts[token]
	if a == nil || a.decision == nil {
		return ""
	}
	return a.decision.Code
}

// TestCleanupFixture installs held physical gates used by cleanup integration tests.
func (c *ControlledExecutor) TestCleanupFixture() (read, stop, dispose <-chan struct{}, holdStop, holdDispose bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	stopGate := &controlledGate{done: make(chan struct{})}
	disposeGate := &controlledGate{done: make(chan struct{})}
	readGate := &controlledGate{done: make(chan struct{})}
	a := &controlledAttempt{holdStop: true, holdDispose: true, stops: []*controlledGate{stopGate}, dispose: disposeGate, read: readGate}
	c.order = append(c.order, a)
	return readGate.done, stopGate.done, disposeGate.done, a.holdStop, a.holdDispose
}

// TestReleaseGates releases every unreleased test-held gate without changing attempt phases.
func (c *ControlledExecutor) TestReleaseGates() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.order {
		a.holdStop, a.holdDispose, a.holdRead = false, false, false
		for _, g := range append(append([]*controlledGate{}, a.stops...), a.dispose, a.read) {
			if g != nil && g.done != nil && !g.released {
				g.result, g.released = GateOK, true
				close(g.done)
			}
		}
	}
	c.signal()
}

// TestAcceptedTokens snapshots attempts whose reports teardown must complete.
func (c *ControlledExecutor) TestAcceptedTokens() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var tokens []uint64
	for _, a := range c.order {
		if a.phase == controlledAccepted {
			tokens = append(tokens, a.launch.Token)
		}
	}
	return tokens
}

// TestPhysicalLive reports active calls or attempts not yet physically retired.
func (c *ControlledExecutor) TestPhysicalLive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, a := range c.order {
		if a.stopActive || a.reads != 0 || a.phase == controlledReported || a.phase == controlledRegistered {
			return true
		}
	}
	return false
}

func (c *ControlledExecutor) TestDisposeHeld() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.order) == 0 {
		return false
	}
	g := c.order[0].dispose
	return g != nil && !g.released
}
