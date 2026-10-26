package lab

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// Test-only teardown runs after assertions/capture: it cannot change the cut
// under test. Even Fatal must leave a synctest bubble with no live Runner or
// physical controlled call (§6.2). Reports remain executor confirmations.
func labTestJoinHarness(t *testing.T, h *RunHarness) {
	t.Helper()
	if h == nil {
		return
	}
	c := h.Controlled
	c.mu.Lock()
	var tokens []uint64
	for _, a := range c.order {
		a.holdStop, a.holdDispose, a.holdRead = false, false, false
		for _, g := range append(append([]*controlledGate{}, a.stops...), a.dispose, a.read) {
			if g != nil && g.done != nil && !g.released {
				g.result, g.released = GateOK, true
				close(g.done)
			}
		}
		if a.phase == controlledAccepted {
			tokens = append(tokens, a.launch.Token)
		}
	}
	c.signal()
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, token := range tokens {
		if err := c.Complete(ctx, token, jobbatch.Result{State: jobbatch.StateInterrupted}); err != nil {
			t.Errorf("teardown report: %s", ErrorNameOf(err))
		}
	}
	if err := h.Runner.Close(ctx); err != nil {
		t.Errorf("teardown Close: %s", ErrorNameOf(err))
	}
	// Close joins logical disposal; this additionally joins actual calls whose
	// logical timeout had already fired.
	c.mu.Lock()
	for {
		live := false
		for _, a := range c.order {
			live = live || a.stopActive || a.reads != 0 || a.phase == controlledReported || a.phase == controlledRegistered
		}
		if !live {
			break
		}
		changed := c.changed
		c.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			t.Error("teardown physical join bound")
			return
		}
		c.mu.Lock()
	}
	c.mu.Unlock()
}
