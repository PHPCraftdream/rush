package agent

// R12-H2 chunk D, acceptance item 6 (design §3 row "Stop/detach", Stop
// half).
//
// Production entry points exercised: the REAL RunnerHandle.Submit (explicit
// nested batch, MaxParallel 1 → two capacity-competing leaves), the REAL
// RunnerHandle.Stop (engine cancellation), the REAL default executor's
// Start (Execution owner) and its physical-Dispose wrapper
// (batch_exec_binding.go — the ONLY execution-owner releaser), with the
// provider blocked INSIDE its WriteFile (gate in the fake provider).
//
// Oracles:
//   - Stop/wait returns (the ack) while the first leaf is still running:
//     the node does NOT settle and the execution owner is not released —
//     the capacity-competing second leaf cannot start.
//   - Releasing the provider gate produces exactly ONE physical settlement
//     (the running leaf A's physical write LANDS on its return — the write
//     is real and asserted); the stopped leaf B is start-rejected and NEVER
//     runs. The engine's durable projection is deterministically CANCELLED
//     for the whole stopped tree: EventStopRequested stamps CauseStop on
//     every non-terminal node (tree_apply.go cancel), and when A's physical
//     EventSettled arrives the engine OVERWRITES the reported outcome state
//     with causeState(CauseStop) = cancelled (engine.go EventSettled). So
//     A settles terminal as "cancelled" even though its write landed —
//     stop-then-physical-return is recorded as cancelled, deterministically.
//   - The Execution owner is released only by the Dispose wrapper: it is
//     still held after the Stop acknowledgement and drops only after the
//     physical settle + Runner.Release + dispose.
//
// Production mutation caught: releasing admission/capability owners on
// cancellation observation alone (instead of after the physical return and
// Dispose) would delete the binding while leaf A's physical I/O is still in
// flight; settling admission at the Stop ack (or letting the competing leaf
// start after cancellation) would run provider work the caller asked to
// stop.
//
// Housekeeping: the test releases the gate, waits for the one physical
// settlement and the terminal root, then releases its Construction owner and
// asserts the owner set empties (and that the borrowed provider's Close was
// never called).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestStopHoldsExecutionUntilPhysicalDispose(t *testing.T) {
	env := newH2aGatedInstall(t)
	env.fake.arm()
	dir := env.targetPath[:len(env.targetPath)-len("out.txt")]
	pathA, pathB := dir+"stop-a.txt", dir+"stop-b.txt"

	leafPayload := func(path, callID string) json.RawMessage {
		raw, err := json.Marshal(batchLeafPayload{
			Tool: "fs_write", ToolCallID: callID, SessionID: env.sessionID,
			Input: json.RawMessage(h2aLeafInput(path)), RegistryID: env.registryID,
			Policy: &batchLeafPolicyContext{
				RegistryID: env.registryID, RestrictedToolset: env.fingerprint,
				CustomDisk: "h2a", DiskProvider: true, DiskBindingID: env.bindingID,
			},
		})
		require.NoError(env.t, err)
		return raw
	}
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{
		Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll, MaxParallel: 1,
		Items: []jobbatch.Spec{
			{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: leafPayload(pathA, "call-stop-a")}},
			{Task: &jobbatch.TaskSpec{Kind: batchKindGeneric, Payload: leafPayload(pathB, "call-stop-b")}},
		},
	}}
	root := installSubmit(t, env.install, spec)

	// Leaf A parks INSIDE the provider WriteFile gate.
	select {
	case <-env.leaf.inFlight:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "leaf A never reached its provider gate")
	}

	// Drive the REAL Stop: the acknowledgement returns while the physical
	// provider I/O is still blocked.
	require.NoError(t, env.install.RunnerHandle.Stop(context.Background(), root))
	require.True(t, h2aAnyNonTerminal(h2aTaskStates(t, env.store, env.sessionID, root)),
		"admission is not settled after the Stop ack while the provider is blocked")
	require.Zero(t, env.fake.writeCount(), "no write lands while the provider gate holds")
	require.Equal(t, 1, env.leaf.runCount(), "only leaf A has run")
	// The Execution owner is still held: the binding stays resolvable after
	// the Stop acknowledgement (only the physical Dispose releases it).
	_, err := env.install.DiskBindings.Lookup(env.bindingID)
	require.NoError(t, err, "the Execution owner must survive the Stop acknowledgement")
	_, err = env.registries.Resolve(env.registryID)
	require.NoError(t, err, "the pinned construction stays alive beneath the execution")

	// Physical return: leaf A's write lands and settles — exactly ONE
	// physical settlement. The stopped leaf B is start-rejected (the engine
	// refuses starts under cancellation) and NEVER runs; the root settles
	// terminal. The durable projection of the whole stopped tree is
	// deterministically cancelled (see the oracle note in the header).
	env.leaf.letGo()
	env.fake.letGo()
	require.Eventually(t, func() bool {
		states := h2aTaskStates(t, env.store, env.sessionID, root)
		return len(states) == 3 && !h2aAnyNonTerminal(states)
	}, 5*time.Second, time.Millisecond,
		"the root settles terminal after A's physical return (root + two leaves)")
	require.Equal(t, 1, env.leaf.runCount(), "the stopped leaf B never started")
	require.Equal(t, 1, env.fake.writeCount(), "exactly one physical provider write")
	got, ok := env.fake.has(pathA)
	require.True(t, ok, "%s written through the custom provider", pathA)
	require.Equal(t, "lifecycle-data", got)
	_, ok = env.fake.has(pathB)
	require.False(t, ok, "the cancelled leaf B never wrote")
	// A's write LANDED, but the engine deterministically projects the stopped
	// tree as cancelled: CauseStop stamped at Stop (tree_apply.go cancel) is
	// reapplied over A's physical report at EventSettled (engine.go). All
	// three nodes settle terminal as "cancelled" — A despite its write,
	// B without ever running.
	states := h2aTaskStates(t, env.store, env.sessionID, root)
	require.Len(t, states, 3, "root + two leaves, all terminal")
	require.NotContains(t, states, "running")
	require.NotContains(t, states, "pending")
	for _, s := range states {
		require.Equal(t, "cancelled", s,
			"stop-then-physical-return projects cancelled deterministically (got %v)", states)
	}

	// REAL release trigger for the lingering Execution owner (R12-H2 §1.2):
	// the root settled terminal, but jobbatch disposes a settled root's
	// execution ONLY on root release — Runner.Release → EventRelease →
	// handle.giveUp → maybeDispose → the physical-Dispose wrapper drops the
	// Execution owner (internal/jobbatch/runner_control.go).
	require.NoError(t, env.install.Runner.Release(context.Background(), root))

	// Housekeeping: settle + dispose released the Execution owner; release
	// the fixture's Construction owner; the owner set empties.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding unavailable after dispose")
	require.Equal(t, 0, env.install.DiskBindings.ownerCount())
	require.Zero(t, env.fake.closeCount(), "provider Close is never called by Rush")
}
