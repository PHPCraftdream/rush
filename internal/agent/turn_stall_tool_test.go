package agent

// Batch rewrite (H4 lane P5): the stall-DETACH tests
// (TestTurnStallDetachReturnsImmediateJobResult,
// TestTurnStallDetachExcludesDelegationAndBackground) were DELETEd with the
// detach authority itself (turn_stall_tool_detach.go, stallDetacherTool, the
// stallDetach* maps, the job_output/job_kill controller): there is no detach
// action left to pin. What survives is the generic stall CLOCK policy:
// TestTurnStallPolicyFiresEveryCrossing guards the PART-1 fix -- the policy
// seam is invoked on CONSECUTIVE threshold crossings inside the 30-minute
// warn window (only the warn+dump is rate-limited). Spacing and zero-timeout
// policy have their own files (turn_stall_policy_spacing_test.go,
// turn_stall_zero_timeout_test.go).

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestTurnStallPolicyFiresEveryCrossing(t *testing.T) {
	t0 := time.Now()
	set := installFakeStallNow(t)
	set(t0)
	sc := &stallClock{abortTimeout: time.Minute}
	sc.markProgress()
	sc.setPhase("tool", "slow_tool", "call-x")
	sessionID := "sess-crossing"
	armedStallClocks.Store(sessionID, sc)
	t.Cleanup(func() { armedStallClocks.Delete(sessionID) })

	var fires atomic.Int64
	prev, _ := turnStallPolicy.Load().(func(string, turnPhase, time.Duration, time.Duration))
	t.Cleanup(func() {
		turnStallPolicy.Store(prev)
	})
	SetTurnStallPolicy(func(sid string, phase turnPhase, sp, sb time.Duration) {
		if phase.kind != "tool" || phase.callID != "call-x" {
			t.Errorf("policy phase = %s/%s, want tool/call-x", phase.kind, phase.callID)
		}
		fires.Add(1)
	})

	// Two consecutive crossings INSIDE the 30-min warn window must both
	// reach the policy (only warn+dump is rate-limited).
	set(t0.Add(11 * time.Minute))
	stallSamplerTickOnce()
	firstWarn := sc.lastWarnNanos.Load()
	set(t0.Add(11*time.Minute + 5*time.Second))
	stallSamplerTickOnce()
	if fires.Load() != 2 {
		t.Fatalf("policy fires = %d, want 2 inside the warn window", fires.Load())
	}
	if sc.lastWarnNanos.Load() != firstWarn {
		t.Fatal("warn+dump was not rate-limited inside the window")
	}
	// Outside the window the warn fires again alongside the policy.
	set(t0.Add(45 * time.Minute))
	stallSamplerTickOnce()
	if fires.Load() != 3 {
		t.Fatalf("policy fires = %d, want 3", fires.Load())
	}
	if sc.lastWarnNanos.Load() == firstWarn {
		t.Fatal("warn did not fire after the window opened")
	}
}
