package lab

import (
	"context"
	"testing"
	"testing/synctest"
)

// Controlled scripts measure logical control deadlines, not host scheduling
// latency. A virtual-time bubble keeps a runnable immediate Dispose from losing
// a 50ms timer to OS descheduling; held calls still time out at the exact bound.
func labTestRunScenario(t *testing.T, name string, s *Scenario, opts RunOptions) (result RunResult) {
	t.Helper()
	if name != "w-c-i" && name != "w-c-ii" {
		return RunScenario(context.Background(), s, opts)
	}
	synctest.Test(t, func(t *testing.T) {
		var owned *RunHarness
		before := opts.BeforeAction
		opts.BeforeAction = func(ctx context.Context, i int, h *RunHarness) error {
			owned = h
			if before != nil {
				return before(ctx, i, h)
			}
			return nil
		}
		defer func() { labTestJoinHarness(t, owned) }()
		result = RunScenario(context.Background(), s, opts)
	})
	return result
}
