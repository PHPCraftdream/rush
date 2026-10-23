package agent

// R12-H2 chunk D, acceptance items 7 and 15 (debt/retention row).
//
// TestDetachedDeliveryRetainsOwners (item 7):
//   - Production entry points: BindCallDiskProvider (the trusted call
//     state's immutable binding handle + Call owner), batchRuntime.Invoke
//     with detached delivery (deliverDetached: ElectStarted + started
//     response, no wait), the real engine, and the physical-Dispose
//     wrapper.
//   - Oracle: the detached SDK/caller return drops ONLY the Call owner —
//     the root and execution owners keep the binding resolvable while the
//     work runs; the work completes and is durable; after settle + dispose
//     and the fixture's Construction release, no provider owner remains.
//   - Mutation caught: a detached return that disposed the call's owners
//     (or an executor that released at Start) would delete the binding
//     beneath the still-running detached work.
//
// TestDetachedDebtRetentionIndependentRelease (item 15, owner-independence
// part):
//   - Production entry points: detached delivery through Invoke, the real
//     Elector (elected started notice = held delivery debt), the durable
//     batch store, and the manager's owner-set release.
//   - Oracle: with the delivery debt held (notice pending, never drained),
//     the required batch rows SURVIVE; and once all executable scopes are
//     closed/disposed (root Done, execution Dispose, construction release)
//     the provider/construction ownership is released INDEPENDENTLY of the
//     debt — binding deleted, owner set empty, rows still present.
//   - Mutation caught: a "delivery debt is an immortal provider owner"
//     implementation (or the inverse: pruning durable rows when ownership
//     ends) breaks either the debt's durability or the release
//     independence.
//
// Housekeeping: both tests release their own registration/Call owners
// before expecting deletion and assert the borrowed provider's Close is
// never called.

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

func TestDetachedDeliveryRetainsOwners(t *testing.T) {
	env := newH2aGatedInstall(t)
	env.leaf.arm() // gate the LEAF: the detached root must stay live past the caller's return
	// Mint the trusted call state's binding handle + Call owner exactly as
	// the SDK invocation setup does (BindCallDiskProvider at registration).
	callDisk := newH2aGatingDisk()
	opts, releaseCall, err := BindCallDiskProvider(env.install.DiskBindings, callDisk, "h2a-detached-call-1", nil)
	require.NoError(t, err)
	bindingID := opts.DiskBindingID
	require.NotEmpty(t, bindingID)
	t.Cleanup(releaseCall) // safety net; the test releases explicitly below

	inv, call, _, hash := h2aGatedInvocation(t, env, bindingID, "call-detached-1", batchDeliveryDetached)
	resp, err := env.install.Runtime.Invoke(context.Background(), inv, env.leaf, call)
	require.NoError(t, err)
	// Detached delivery returned the started response while the work runs.
	require.True(t, strings.HasPrefix(resp.Content, "batch root ") && strings.HasSuffix(resp.Content, batchStartedResponseSuffix),
		"detached delivery must return the started response, got %q", resp.Content)

	// SDK/caller return: only the Call owner drops.
	releaseCall()
	root, attached, err := env.install.Attach.ProbeLive(context.Background(), h2aProbe(env, hash, "call-detached-1"))
	require.NoError(t, err)
	require.True(t, attached, "the detached root stays live after the caller returns")
	_, err = env.install.DiskBindings.Lookup(bindingID)
	require.NoError(t, err, "root/execution owners keep the binding after the Call owner drops")
	require.True(t, h2aAnyNonTerminal(h2aTaskStates(t, env.store, env.sessionID, root)),
		"the detached work is still running when the caller is gone")

	// The work completes through the real engine and is durable: release the
	// leaf gate; the write lands on the call's OWN provider.
	env.leaf.letGo()
	require.Eventually(t, func() bool {
		stored, ok, err := env.install.Done.LoadDone(context.Background(), env.sessionID, root)
		return err == nil && ok && !stored.IsError
	}, 5*time.Second, time.Millisecond, "the detached completion is durable")
	got, ok := callDisk.has(env.targetPath)
	require.True(t, ok, "the detached execution wrote through ITS OWN provider instance")
	require.Equal(t, "lifecycle-data", got)
	require.Zero(t, env.fake.writeCount(), "the detached call never routed to the fixture provider")

	// REAL release trigger for the lingering Execution owner (R12-H2 §1.2):
	// the root settled, and the adopted Done watcher (batchRootLeaseStore
	// .adopt) calls the Runner's Release AUTOMATICALLY once Done closes —
	// no manual Runner.Release for the adopted root. The physical-Dispose
	// wrapper drops the Execution owner
	// (internal/jobbatch/runner_control.go).

	// After settle + dispose the root/execution owners are gone; release the
	// fixture's Construction owner and the binding disappears.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding deleted once no legitimate owner remains")
	require.Equal(t, 0, env.install.DiskBindings.ownerCount())
	require.Zero(t, env.fake.closeCount())
	require.Zero(t, callDisk.closeCount())
}

func TestDetachedDebtRetentionIndependentRelease(t *testing.T) {
	env := newH2aGatedInstall(t)
	inv, call, _, hash := h2aGatedInvocation(t, env, env.bindingID, "call-debt-1", batchDeliveryDetached)
	resp, err := env.install.Runtime.Invoke(context.Background(), inv, env.leaf, call)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(resp.Content, batchStartedResponseSuffix), "detached started response")
	root, attached, err := env.install.Attach.ProbeLive(context.Background(), h2aProbe(env, hash, "call-debt-1"))
	require.NoError(t, err)
	require.True(t, attached, "live before settlement")

	// The work completes; the delivery debt is the elected started notice
	// (deliverDetached elected it at return) — held, never drained. A second
	// election loses without error, proving the durable notice exists.
	env.leaf.letGo() // no provider gate: the leaf completes immediately
	require.Eventually(t, func() bool {
		_, ok, err := env.install.Done.LoadDone(context.Background(), env.sessionID, root)
		return err == nil && ok
	}, 5*time.Second, time.Millisecond, "the detached work completes")
	won, err := env.install.Elector.ElectStarted(context.Background(), env.sessionID, root)
	require.NoError(t, err)
	require.False(t, won, "the detached return already elected the delivery debt — it is durably held, never drained")

	// Required rows survive the debt: capture the FULL durable snapshot
	// (root + every node row, including delivery/debt columns) before the
	// retention machinery runs.
	ctx := context.Background()
	snapBefore, err := env.store.Read(ctx, env.sessionID, string(root))
	require.NoError(t, err)
	require.NotEmpty(t, snapBefore.Nodes)

	// Review P1-7d: invoke the REAL retention machinery while the delivery
	// debt is unresolved. A cutoff far in the future selects everything
	// that is eligible; a debt-protected root must be selected ZERO times
	// and purged ZERO times, and every durable row must be byte-identical
	// across the purge (same states AND same delivery/debt columns).
	farCutoff := time.Now().Add(time.Hour).Unix()
	expired, err := env.store.BatchCountExpired(ctx, farCutoff)
	require.NoError(t, err)
	require.Zero(t, expired, "a root with unresolved delivery debt is not retention-eligible")
	purged, err := env.store.BatchPurgeExpired(ctx, farCutoff)
	require.NoError(t, err)
	require.Zero(t, purged, "the retention purge must not delete a debt-protected root")
	snapHeld, err := env.store.Read(ctx, env.sessionID, string(root))
	require.NoError(t, err)
	require.Equal(t, snapBefore, snapHeld,
		"the durable rows (root+nodes, delivery/debt columns included) survive the retention sweep while the debt is unresolved")

	// Close/dispose every executable scope: the settled root's execution is
	// disposed when the root is RELEASED (R12-H2 §1.2) and the adopted Done
	// watcher releases it AUTOMATICALLY — no manual Runner.Release for the
	// adopted root. The fixture then releases its Construction owner;
	// provider/construction ownership is released INDEPENDENTLY of the held
	// debt — and the durable rows survive the release.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding ownership released despite the held debt")
	require.Equal(t, 0, env.install.DiskBindings.ownerCount())
	// The store exposes no artificial debt-resolution path (delivery debt is
	// resolved by consumption, never by ownership events), so the debt stays
	// held and retention must STILL not delete the rows: ownership release
	// does not enable deletion of debt-protected rows.
	snapAfter, err := env.store.Read(ctx, env.sessionID, string(root))
	require.NoError(t, err)
	require.Equal(t, snapBefore, snapAfter,
		"the rows are byte-identical after the ownership release")
	expiredAfter, err := env.store.BatchCountExpired(ctx, farCutoff)
	require.NoError(t, err)
	require.Zero(t, expiredAfter,
		"an implementation tying provider/construction ownership to durable debt would make the rows deletable once owners end — they must not be")
	purgedAfter, err := env.store.BatchPurgeExpired(ctx, farCutoff)
	require.NoError(t, err)
	require.Zero(t, purgedAfter, "retention never deletes debt rows")
	// The debt is STILL held after the ownership release: the durable started
	// election remains claimed, and it granted no immortal provider owner.
	stillHeld, err := env.install.Elector.ElectStarted(context.Background(), env.sessionID, root)
	require.NoError(t, err)
	require.False(t, stillHeld, "the delivery debt survives the ownership release")
	require.Zero(t, env.fake.closeCount(), "provider Close is never called by Rush")
}

// Compile-time guard: the detached call's provider is a real tools provider.
var _ tools.DiskProvider = (*h2aGatingDisk)(nil)

var _ fantasy.AgentTool = (*lifecycleCaptureLeaf)(nil)
