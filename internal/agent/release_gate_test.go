package agent

// Release Gate Test Suite for Tasks #337-347
//
// This file implements 9 release gate tests that prove the fixes for the
// concurrency review rounds are production-ready. All tests follow the
// "no external poke" rule: they wait for autonomous mechanisms (pump,
// OS lock timeout, real context cancellation) instead of manually triggering
// Run()/startDetachedRun() in the second phase.
//
// CRITICAL DESIGN RULE:
// Every test must let the production mechanisms run autonomously.
// Acceptable: session.RunQueuePump with TestTick (pump still autonomous, just faster)
// Unacceptable: Test calling Run()/startDetachedRun() in second phase to "unblock" scenario
//
// Run the entire suite with:
//   go test -run TestReleaseGate ./internal/agent/... ./internal/app/... -v

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/session"
)

// p0338PumpCoordinator adapts session.SessionAgentCallData to agent.SessionAgentCall
// and executes it through the exported agent.SessionAgent interface.
type p0338PumpCoordinator struct {
	sessionAgent SessionAgent
}

func (p *p0338PumpCoordinator) Run(ctx context.Context, callData session.SessionAgentCallData) (*any, error) {
	call, err := FromSessionAgentCallData(callData)
	if err != nil {
		return nil, err
	}
	// Mirror production's coordinator.RebuildSessionAgentCall (coordinator.go):
	// mark this call as originating from the durable queue so mailbox.submit
	// skips mb.submitted for it (P0-1, closing-review round). Without this,
	// this test-only conversion path diverges from production behavior in a
	// way that became relevant only once P0-1 landed.
	call.FromDurableQueue = true
	result, err := p.sessionAgent.Run(ctx, call)
	if err != nil {
		return nil, err
	}
	var anyResult any
	if result != nil {
		anyResult = result
	}
	return &anyResult, nil
}
