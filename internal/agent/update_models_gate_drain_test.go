package agent

// P1 (role-models hot reload, round 5): UpdateModels must drain the shared
// readyWg gate it fills before returning.
//
// buildTools (via agentTool -> buildAgent) registers the system-prompt and
// tool-build tasks on the SHARED gate, bound to UpdateModels' own ctx. Every
// caller cancels that ctx right after UpdateModels returns (the poller
// rebuild's `defer cancel()` at internal/app/app_config_sync.go:55 is the
// production case). Before the fix UpdateModels returned with tasks still
// pending, the cancellation landed under them, and the tasks returned
// context.Canceled into the STICKY gate (ready_gate.go:83-85, 105-114) — so the
// NEXT run's entry-point readyWg.Wait (coordinator.go:544) failed with
// context.Canceled before any provider request, and the app stamped the
// turn "canceled". That is exactly the CI failure
// (TestRoleModelHotReload_NextTurnUsesNewSmart: "no provider request within
// 30s; turn 2 outcome err=run did not complete cleanly (canceled)").
//
// Mutation caught: reverting coordinator_models_build.go's tail to
// `return nil` (no gate drain) makes UpdateModels return while its build
// tasks are still pending. With the held-task harness below (a task parked
// on the gate across the whole call) the mutant fails the "UpdateModels has
// NOT returned while the task is held" assertion immediately; with the
// production code, cancel() only lands once the gate's pending count has
// drained to exactly the held task (event-derived, not assumed), so no
// build task ever observes cancellation and the sticky gate stays clean.
//
// Precision: the gate keeps errors STICKY (ready_gate.go:83-85 and 105-114 —
// Wait returns the FIRST error any task ever reported). The fix here does
// NOT clear or prevent build-failure stickiness; it only prevents the
// caller's deferred cancellation from being deferred UNDER still-pending
// tasks, i.e. it prevents deferred-cancel poisoning of later runs. A real
// build failure still surfaces as UpdateModels' own error and stays sticky.

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestUpdateModelsDrainsReadyGateBeforeReturning(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)
	// UpdateModels publishes onto the shared currentAgent; the bare
	// newToolPinningCoordinator literal leaves it nil (same as
	// TestUpdateModels_NeverPublishesPerCallToolFilter, which swaps in a
	// recorder). A recording mock keeps the publish observable and the
	// Set* calls non-panicking.
	coord.currentAgent = &toolPublishRecorder{mockSessionAgent: newMockAgent("smart-provider", 4096,
		func(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
			return agentResultWithText("ok"), nil
		})}

	// ctx mirrors the poller rebuild's shape: alive across the call,
	// canceled by the caller once the rebuild is done (in production via
	// `defer cancel()` at app_config_sync.go:54-56; here explicitly, while
	// the held task still parks the gate, to exercise the same
	// cancel-under-gate ordering).
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Scheduling control: park a blocking task on the SAME gate before
	// UpdateModels runs. readyGate.Go tolerates registrations concurrent
	// with Wait (ready_gate.go:71-93), so this is legal even though
	// UpdateModels will call readyWg.Wait immediately after its build tasks
	// are registered. While `held` is open, UpdateModels (with the fix)
	// must be blocked in readyWg.Wait behind this task. The release is
	// registered as a Cleanup BEFORE any assertion, so an early t.Fatalf
	// (e.g. the not-returned select below) cannot leak the parked task;
	// sync.Once makes the cleanup's close safe next to the explicit one.
	held := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(held) }) })
	coord.readyWg.Go(func() error { <-held; return nil })

	// Run UpdateModels in a goroutine. With the fix (tail
	// `return c.readyWg.Wait()`), it must NOT return while `held` is
	// closed-and-blocking; the `return nil` mutant returns immediately.
	returned := make(chan error, 1)
	go func() { returned <- coord.UpdateModels(ctx) }()
	select {
	case err := <-returned:
		t.Fatalf("UpdateModels returned while a gate task was still held: got err=%v — the gate drain was reverted (coordinator_models_build.go tail must be `return c.readyWg.Wait()`)", err)
	case <-time.After(2 * time.Second):
		// expected: UpdateModels is parked on readyWg.Wait()
	}

	// Cancel the ctx ONLY once the held task is provably the LAST pending
	// one: a bounded, event-derived wait on the gate's pending count. The
	// 2s bounded select above only tells us UpdateModels has not returned;
	// it does NOT prove its own build tasks finished — a still-running real
	// prompt/tool build task would see the cancellation (prompt.go:746-752)
	// and stick context.Canceled into the sticky gate
	// (ready_gate.go:83-85), failing correct production. Waiting until
	// pendingCount() == 1 (1 = the held task, which ignores ctx) is the
	// event that certifies every build task has returned; then cancel()
	// lands under the held task alone, exactly like the production
	// deferred-cancel-under-gate ordering. After release, the gate drains
	// with no sticky error.
	require.Eventually(t, func() bool { return coord.readyWg.pendingCount() == 1 },
		10*time.Second, 5*time.Millisecond,
		"only the held task may remain pending before cancel")
	cancel()
	releaseOnce.Do(func() { close(held) })

	select {
	case err := <-returned:
		require.NoError(t, err,
			"UpdateModels must succeed and leave no pending gate task behind")
	case <-time.After(10 * time.Second):
		t.Fatal("UpdateModels did not return within 10s after the gate task was released")
	}

	// With the fix, the gate is EMPTY at return (UpdateModels waited on it
	// under the still-alive ctx), so no task can be canceled retroactively
	// and Wait returns immediately with no sticky error.
	done := make(chan error, 1)
	go func() { done <- coord.readyWg.Wait() }()
	select {
	case err := <-done:
		require.NoError(t, err,
			"the ready gate must be clean after UpdateModels returned: a canceled build task poisoned it (UpdateModels did not drain the gate before returning)")
	case <-time.After(10 * time.Second):
		t.Fatal("readyWg.Wait did not return within 10s after UpdateModels returned: the gate is still pending")
	}

	// Same contract from the next consumer's point of view: a follow-up
	// UpdateModels (the next poll tick / the next turn's shared-agent
	// refresh) must succeed on the drained gate instead of inheriting a
	// sticky cancellation-class error.
	require.NoError(t, coord.UpdateModels(t.Context()),
		"a follow-up shared-agent rebuild must not inherit the previous build's poisoned gate")
}
