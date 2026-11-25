package lab

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// Test-only teardown runs after assertions/capture: it cannot change the cut
// under test. Even Fatal must leave a synctest bubble with no live Runner or
// physical controlled call (§6.2). Reports remain executor confirmations.
func labTestJoinHarness(t *testing.T, h *RunHarness) {
	t.Helper()
	if h == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c := h.Controlled
	c.TestReleaseGates()
	for _, token := range c.TestAcceptedTokens() {
		if err := c.Complete(ctx, token, jobbatch.Result{State: jobbatch.StateInterrupted}); err != nil {
			t.Errorf("teardown report: %s", ErrorNameOf(err))
		}
	}
	if err := h.Runner.Close(ctx); err != nil {
		t.Errorf("teardown Close: %s", ErrorNameOf(err))
	}
	for c.TestPhysicalLive() {
		select {
		case <-ctx.Done():
			t.Error("teardown physical join bound")
			return
		case <-time.After(time.Millisecond):
		}
	}
}
