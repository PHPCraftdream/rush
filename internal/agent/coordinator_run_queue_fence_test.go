package agent

// CI-only tests for the R12-H2b §2.4 fence/owner choreography: the
// ordinary-turn fence guard, the row-keyed Continuation holder held by a
// durable enqueue producer and released by the pump's rebuild, and the
// fail-closed terminal classification of a row whose binding is gone.
// Each test names the production mutation it catches: deleting or
// weakening the guarded code path would let an ordinary turn silently
// rebuild a fenced session onto OS disk, release a holder under the
// wrong key, or pin a binding until process exit.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuardTurnDiskFence_RefusesOptionslessTurnOnFencedSession catches
// two mutations at once: (1) deleting guardTurnDiskFence's refusal (or
// its fence resolution) lets an ordinary web/CLI turn silently rebuild a
// fenced session onto OS disk (design §2.4); (2) an over-broad guard
// that refuses even an explicit re-supply would deadlock a session whose
// operator re-established the binding with a fresh SDK run — the
// explicit binding is a new epoch and must pass, and an unfenced session
// must stay on the ordinary path. It also catches the explicit canonical
// OS provider (sdk.OSDiskProvider()): a deliberate operator choice with
// NO binding to name must pass the guard on a fenced session even when
// the stored fence names a DEAD binding (the CI regression shape); a
// guard that consults the stale fence in that case fails the run with
// ErrDiskProviderUnavailable.
func TestGuardTurnDiskFence_RefusesOptionslessTurnOnFencedSession(t *testing.T) {
	ctx := context.Background()
	caps := newDiskCapabilityManager()
	bindingA, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)
	bindingB, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)
	// The live SDK runs that own both bindings: a binding with an empty
	// owner set is pruned by the first release, so each guard call below
	// must find its binding retained by a Call owner, as in production.
	for _, id := range []string{bindingA, bindingB} {
		key := diskOwnerKey{kind: diskOwnerCall, instance: "sdk-run-" + id, bindingID: id}
		require.NoError(t, caps.acquire(key))
		t.Cleanup(func() { caps.release(key) })
	}
	store := &fakeFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(ctx, "s1",
		session.SessionExecutionRequirement{BindingID: bindingA}))
	coord := newContinuationTestCoordinator(store, caps)

	// A fenced session and an optionsless turn: no live provider can be
	// named, so the turn must be refused typed, not silently run on disk.
	err = coord.guardTurnDiskFence(ctx, "s1", nil)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)

	// The same session with an explicit re-supply naming the fence's own
	// binding passes.
	err = coord.guardTurnDiskFence(ctx, "s1", &CallOptions{
		DiskBindingID: bindingA,
		Requirement:   &session.SessionExecutionRequirement{BindingID: bindingA},
	})
	require.NoError(t, err)

	// A different live binding is a new epoch (an explicit run re-bound
	// the session); its own binding supersedes, so it passes too.
	err = coord.guardTurnDiskFence(ctx, "s1", &CallOptions{
		DiskBindingID: bindingB,
		Requirement:   &session.SessionExecutionRequirement{BindingID: bindingB},
	})
	require.NoError(t, err)

	// An EXPLICIT canonical OS provider on the options is a deliberate
	// operator choice (sdk.OSDiskProvider()): a new epoch that supersedes
	// the fence at admission. Here the stored fence names a binding whose
	// owner has ALREADY been released (dead binding): the guard must pass
	// without consulting the stale fence.
	bindingC, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)
	require.NoError(t, store.DeleteSessionExecutionRequirement(ctx, "s1"))
	require.NoError(t, store.StoreSessionExecutionRequirement(ctx, "s1",
		session.SessionExecutionRequirement{BindingID: bindingC}))
	// Release bindingC's sole owner: the binding prunes and the fence
	// names a dead binding from this point on.
	caps.release(diskOwnerKey{kind: diskOwnerCall, instance: "sdk-run-" + bindingC, bindingID: bindingC})
	_, err = caps.Lookup(bindingC)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable,
		"the fixture must model the stale-fence state: bindingC's binding must be dead")
	err = coord.guardTurnDiskFence(ctx, "s1", &CallOptions{DiskProvider: tools.OSDisk()})
	require.NoError(t, err)

	// A session with NO fence (nothing stored for it) and a nil-opts
	// turn is the ordinary OS-disk path: the guard must not block it.
	err = coord.guardTurnDiskFence(ctx, "s-unfenced", nil)
	require.NoError(t, err)
}

// TestHoldDurableContinuationOwner_KeyedByRowID catches the holder being
// released under the wrong key: the pump's release must match the
// producer's row key exactly, or the row-keyed owner survives forever
// (binding pinned until process exit) or vanishes early (binding pruned
// mid-wait, the row can then never succeed).
func TestHoldDurableContinuationOwner_KeyedByRowID(t *testing.T) {
	caps := newDiskCapabilityManager()
	bindingID, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)

	release, ok := holdDurableContinuationOwner(caps, bindingID, "row-1")
	require.True(t, ok, "a live binding must be holdable")
	_, err = caps.Lookup(bindingID)
	require.NoError(t, err, "the holder must retain the binding")

	// A release under a DIFFERENT row key must not touch the holder.
	releaseDurableContinuationOwner(caps, bindingID, "row-2")
	_, err = caps.Lookup(bindingID)
	require.NoError(t, err, "the binding must stay held under row-1")

	// The release under the SAME row key drops the holder (the binding
	// prunes, since no other owner exists).
	release()
	_, err = caps.Lookup(bindingID)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)

	// Double release is a no-op (idempotent once-guard).
	release()
	releaseDurableContinuationOwner(caps, bindingID, "row-1")
	assert.Equal(t, 0, caps.ownerCount())

	// An unknown binding fails closed with ok=false.
	_, ok = holdDurableContinuationOwner(caps, "never-registered", "row-3")
	assert.False(t, ok, "holding a lost binding must be refused")
}

// TestRebuildSessionAgentCall_EnvelopedRowWithDeadBinding_TerminalUnavailable
// catches a restart retrying a durable row whose binding is gone forever:
// the rebuild must fail TERMINAL (ErrCallAlreadyAttempted, so the pump
// dead-letters the row) with the cause classifiable as
// ErrDiskProviderUnavailable — a retryable failure would make the pump
// spin on a row that can never succeed differently.
func TestRebuildSessionAgentCall_EnvelopedRowWithDeadBinding_TerminalUnavailable(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)
	// No providers are ever registered: the envelope's binding cannot
	// resolve in this process.
	coord.diskCaps = newDiskCapabilityManager()

	data := session.SessionAgentCallData{
		SessionID:     "row-dead-binding",
		LogicalCallID: "logical-dead-1",
		Prompt:        "hello",
		DiskRequirement: &session.SessionExecutionRequirement{
			BindingID: "gone",
		},
	}

	call, err := coord.RebuildSessionAgentCall(t.Context(), "row-dead", data)
	require.Error(t, err)
	require.Equal(t, SessionAgentCall{}, call)
	var alreadyAttempted *ErrCallAlreadyAttempted
	require.ErrorAs(t, err, &alreadyAttempted,
		"the dead-binding refusal must be terminal so the pump dead-letters the row instead of spinning")
	require.ErrorIs(t, alreadyAttempted.Err, ErrDiskProviderUnavailable,
		"the wrapped cause must stay classifiable as disk-provider-unavailable")
}

// TestRebuildSessionAgentCall_ReleasesRowKeyedHolderOnSuccess catches the
// success-path transfer leaking the pin: on a successful rebuild the
// row-keyed Continuation holder must be released (the rebuilt call's own
// Call owner takes over retention) — deleting the transfer's release
// would leave a stale Continuation owner pinning the binding until
// process exit.
func TestRebuildSessionAgentCall_ReleasesRowKeyedHolderOnSuccess(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)
	caps := newDiskCapabilityManager()
	coord.diskCaps = caps
	bindingID, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)

	// The durable producer's holder, keyed by the row's durable id.
	require.NoError(t, caps.acquire(diskOwnerKey{
		kind: diskOwnerContinuation, instance: "row-live", bindingID: bindingID,
	}))
	require.Equal(t, 1, continuationOwnerCount(caps))
	before := caps.ownerCount()

	data := session.SessionAgentCallData{
		SessionID:     "row-live-binding",
		LogicalCallID: "logical-live-1",
		Prompt:        "hello",
		DiskRequirement: &session.SessionExecutionRequirement{
			BindingID: bindingID,
		},
	}

	_, err = coord.RebuildSessionAgentCall(t.Context(), "row-live", data)
	require.NoError(t, err)

	// The Continuation owner is gone; the rebuilt call's Call owner now
	// retains the binding.
	assert.Equal(t, 0, continuationOwnerCount(caps),
		"the row-keyed holder must be released once the transfer completes")
	assert.Equal(t, 1, callOwnerCount(caps),
		"the rebuilt call's Call owner must hold the binding")
	assert.Equal(t, before, caps.ownerCount(),
		"the holder is swapped one-for-one for the rebuilt call's Call owner: net owner count unchanged")
	_, err = caps.Lookup(bindingID)
	require.NoError(t, err, "the binding must stay live under the Call owner")

	// A pump-style release of the holder after the transfer is a no-op,
	// however many times it runs.
	releaseDurableContinuationOwner(caps, bindingID, "row-live")
	releaseDurableContinuationOwner(caps, bindingID, "row-live")
	assert.Equal(t, 0, continuationOwnerCount(caps))
	assert.Equal(t, 1, callOwnerCount(caps))
}

// TestRebuildSessionAgentCall_ReleasesRowKeyedHolderOnTerminal catches
// the terminal path leaking the pin: a rebuild that refuses a row must
// release the row-keyed holder (the row is dead — a deleted row must
// never leave an immortal owner). Reproduction note: the dead-binding
// refusal cannot leak a holder it never had (a binding prunes only once
// its holder is gone), so the reproducible terminal refusal is used
// here: the same pre-acquired row-keyed holder dies with the row's
// terminal CallOptionsSpec refusal.
func TestRebuildSessionAgentCall_ReleasesRowKeyedHolderOnTerminal(t *testing.T) {
	env := testEnv(t)
	coord := newRoleModelTestCoordinator(t, env, false)
	caps := newDiskCapabilityManager()
	coord.diskCaps = caps
	bindingID, err := caps.RegisterProvider(newFakeDiskProvider(nil))
	require.NoError(t, err)
	require.NoError(t, caps.acquire(diskOwnerKey{
		kind: diskOwnerContinuation, instance: "row-term", bindingID: bindingID,
	}))
	before := caps.ownerCount()

	data := session.SessionAgentCallData{
		SessionID:     "row-term-binding",
		LogicalCallID: "logical-term-1",
		Prompt:        "hello",
		DiskRequirement: &session.SessionExecutionRequirement{
			BindingID: bindingID,
		},
		// An incompatible spec version forces the terminal refusal after
		// the fence was resolved and the holder was held.
		CallOptionsSpec: &session.CallOptionsSpec{Version: 999},
	}

	_, err = coord.RebuildSessionAgentCall(t.Context(), "row-term", data)
	require.Error(t, err)

	assert.Equal(t, before-1, caps.ownerCount(),
		"the failed rebuild must not leave the pre-acquired row-keyed holder behind")
	assert.Equal(t, 0, continuationOwnerCount(caps))
	_, err = caps.Lookup(bindingID)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable,
		"with the row dead, the binding must prune once the holder is released")
}

// TestHoldDurableContinuationOwner_NilCapsAndEmptyArgs catches a panic
// on bare fixtures: nil caps, an empty binding id or an empty row key
// must return (nil, false) — and releaseDurableContinuationOwner with
// the same must be a safe no-op — never a nil dereference.
func TestHoldDurableContinuationOwner_NilCapsAndEmptyArgs(t *testing.T) {
	release, ok := holdDurableContinuationOwner(nil, "b1", "row-1")
	assert.False(t, ok)
	assert.Nil(t, release)
	_, ok = holdDurableContinuationOwner(newDiskCapabilityManager(), "", "row-1")
	assert.False(t, ok)
	_, ok = holdDurableContinuationOwner(newDiskCapabilityManager(), "b1", "")
	assert.False(t, ok)

	releaseDurableContinuationOwner(nil, "b1", "row-1")
	releaseDurableContinuationOwner(newDiskCapabilityManager(), "", "row-1")
	releaseDurableContinuationOwner(newDiskCapabilityManager(), "b1", "")
}

// TestRebuildSessionAgentCall_UnmarkedRowOnFencedSession_TerminalWithoutLiveBinding
// catches an unmarked row silently restarting on OS disk after a restart
// (the exact promotion the fence exists to prevent): a row with NO
// DiskRequirement and NO HostDiskProvider mark on a session whose
// persisted metadata carries a fence must still resolve the fence — with
// an empty capability manager (the restart state, no live bindings) the
// resolution fails and the rebuild must be TERMINAL and typed as
// ErrDiskProviderUnavailable, so the pump dead-letters instead of
// running the turn on the real disk.
func TestRebuildSessionAgentCall_UnmarkedRowOnFencedSession_TerminalWithoutLiveBinding(t *testing.T) {
	ctx := t.Context()
	caps := newDiskCapabilityManager()
	store := &fakeFenceStore{}
	require.NoError(t, store.StoreSessionExecutionRequirement(ctx, "s-unmarked-fenced",
		session.SessionExecutionRequirement{BindingID: "binding-gone-with-the-first-process"}))
	// newRoleModelTestCoordinator wires no execReqs store, so the
	// fixture here is the minimal continuation one: an EMPTY capability
	// manager models the restart state (no provider registered in this
	// process) while the fence store still names the old binding.
	coord := newContinuationTestCoordinator(store, caps)

	data := session.SessionAgentCallData{
		SessionID:     "s-unmarked-fenced",
		LogicalCallID: "logical-unmarked-1",
		Prompt:        "CONTINUATION_UNMARKED_MARKER",
	}

	call, err := coord.RebuildSessionAgentCall(ctx, "row-unmarked", data)
	require.Error(t, err)
	require.Equal(t, SessionAgentCall{}, call)
	var alreadyAttempted *ErrCallAlreadyAttempted
	require.ErrorAs(t, err, &alreadyAttempted,
		"the unmarked-row refusal must be terminal so the pump dead-letters the row instead of spinning")
	require.ErrorIs(t, alreadyAttempted.Err, ErrDiskProviderUnavailable,
		"the wrapped cause must stay classifiable as disk-provider-unavailable")
}

// Compile-time guard: the envelope classification this file relies on.
var _ error = (*ErrCallAlreadyAttempted)(nil)
