package agent

// Package L round 2 unit coverage for the SUBMIT-TIME retained build scope:
// arming (armQueuedBuildRetention), the promotion-time usability oracle, the
// lock-refusal zero-leak path, and the no-retention guard matrix. The
// queue-exit / orphan / policy coverage lives in
// queued_call_scope_exit_test.go.

import (
	"context"
	"sync/atomic"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newQueuedCallScopeFixture builds an isolated capability manager bound to a
// fresh agent plus a mailbox map, mirroring the minimal state Run's queued
// admission path touches.
func newQueuedCallScopeFixture(t *testing.T) (*DiskProviderRegistry, *sessionAgent) {
	t.Helper()
	caps := NewDiskProviderRegistry()
	a := &sessionAgent{}
	a.bindDiskCaps(caps)
	a.mailboxes = csync.NewMap[string, *mailbox]()
	return caps, a
}

// pinnedIngressTool models a PINNED batch tool: a zero batchIngressTool with
// a populated inv identity (bind == nil is the pinned build's marker).
func pinnedIngressTool(registryID string) *batchIngressTool {
	// The embedded AgentTool carries a real Info(): the real-turn test hands
	// this tool to a live Run, whose step preparation reads Info().Name
	// (stepToolCapabilities); a nil embedded interface would panic there.
	return &batchIngressTool{AgentTool: fantasy.NewAgentTool("pinned_probe", "pinned ingress probe",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.ToolResponse{}, nil
		}), inv: BatchInvocation{
		RegistryID: registryID,
		Policy:     batchLeafPolicyContext{RegistryID: registryID},
	}}
}

// TestQueuedCallScope_PromotionKeepsConstructionUsable drives the REAL Run
// queueing path end to end: a queued admission arms a retained scope, the
// sender's scope closes, the real drainOrRelease pop hands the call back,
// and the construction is still usable at promotion time. What is NOT
// exercised: full runTurn/provider execution and the batch ingress — only
// the real queueing path, the real drain pop, and the real release helper.
//
// MUTATION CAUGHT — removing the arm call in submit (or releasing at arm
// time) makes senderScope.close() prune the construction, so the root
// acquire at promotion fails ErrDiskProviderUnavailable.
func TestQueuedCallScope_PromotionKeepsConstructionUsable(t *testing.T) {
	t.Parallel()
	caps, a := newQueuedCallScopeFixture(t)

	sid := "s-promo"
	holdCtx, epoch, cancel, ok := a.ReserveExclusive(context.Background(), sid)
	require.True(t, ok)
	require.NotNil(t, holdCtx)
	t.Cleanup(func() { a.ReleaseExclusive(sid, epoch, cancel) })

	// The sender's build: pin the construction into a scope the sender
	// closes on return; the test controls that close.
	senderScope := newBuildResourceScope()
	// Fixture acquire (not production-facing): establishes the sender-side
	// construction owner the queued call must outlive.
	require.NoError(t, caps.acquireConstruction("", "reg-p1", "constr-p1"))
	senderScope.register(func() {
		caps.release(diskOwnerKey{kind: diskOwnerConstruction, instance: "constr-p1", registryID: "reg-p1"})
	})

	opts := &CallOptions{buildScope: senderScope}
	call := SessionAgentCall{
		SessionID:   sid,
		Prompt:      "queued",
		CallOptions: opts,
		Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-p1")},
	}

	owners0, con0 := caps.ownerCounts()
	require.Equal(t, 1, owners0)
	require.Equal(t, 1, con0)

	// The session is owned by the fixture's reservation, so Run can only
	// acknowledge the call as queued: (nil, nil).
	res, err := a.Run(context.Background(), call)
	require.NoError(t, err)
	require.Nil(t, res)

	mb := a.getMailbox(sid)
	require.Len(t, mb.submitted, 1)
	// FIRST PRODUCTION-FACING ASSERTION: the queued copy carries the
	// retained marker on its OWN options copy.
	require.True(t, mb.submitted[0].CallOptions.buildScopeRetained)
	require.NotSame(t, opts, mb.submitted[0].CallOptions)
	owners1, con1 := caps.ownerCounts()
	require.Equal(t, owners0+1, owners1)
	require.Equal(t, con0, con1)

	// The sender returns and its scope closes.
	senderScope.close()

	// The REAL pop the dispatcher's drain calls — drainOrReleaseFinal is
	// the primitive every live drain path uses via drainOrReleaseMerged
	// (agent_ownership.go:245-261). What remains unexercised here is
	// runTurn/provider execution and the batch leaf ingress.
	next, hasNext, orphaned, relErr := mb.drainOrReleaseFinal(epoch, nil)
	require.True(t, hasNext)
	require.Len(t, orphaned, 0)
	require.NoError(t, relErr)

	// At promotion time the queued call's construction is still usable: the
	// first batch tool's root acquire oracle.
	require.NoError(t, caps.acquire(diskOwnerKey{kind: diskOwnerRoot, instance: "root-p1", registryID: "reg-p1"}))

	// Turn end: the production release rule (releaseTurnEndBuildScope), in
	// the FIRST-call shape the manual-compaction handoff produces: the
	// dispatcher's origin IS the retained copy. The retained-marker rule
	// must close the scope and clear the marker.
	ownersRoot, _ := caps.ownerCounts()
	releaseTurnEndBuildScope(&next, next.CallOptions)
	require.False(t, next.CallOptions.buildScopeRetained)
	ownersAfterTurnEnd, _ := caps.ownerCounts()
	require.Equal(t, ownersRoot-1, ownersAfterTurnEnd)
	caps.release(diskOwnerKey{kind: diskOwnerRoot, instance: "root-p1", registryID: "reg-p1"})
	// The helper already released the retained scope, so the remaining
	// counts (the root owner was released above, the sender's scope closed
	// earlier) must still reach the empty baseline: 0 owners, 0 constructions.
	owners2, con2 := caps.ownerCounts()
	require.Equal(t, 0, owners2)
	require.Equal(t, 0, con2)

	// Exactly once: a second release stays at the baseline.
	releaseQueuedBuildOwnership(&next)
	owners3, con3 := caps.ownerCounts()
	require.Equal(t, 0, owners3)
	require.Equal(t, 0, con3)
}

// TestQueuedCallScope_AdmissionRefusalLeavesNoOwner pins round-1 flaw A's
// fix: a call that WINS the mailbox reservation and is then refused the
// inter-process lock returns from runOwned with NO retained owner leaked.
//
// MUTATION CAUGHT — arming before tryReserveSession (or arming for winners)
// leaves +1 owner after the refusal.
func TestQueuedCallScope_AdmissionRefusalLeavesNoOwner(t *testing.T) {
	t.Parallel()
	caps := NewDiskProviderRegistry()
	dataDir := t.TempDir()
	a := newLockTestSessionAgent(dataDir, false)
	a.bindDiskCaps(caps)

	sid := "s-refusal"
	// Hold the inter-process lock first: Run's own acquisition must lose.
	lk, err := session.TryAcquireSessionLockWithOptions(dataDir, sid)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lk.Release() })

	// The pin makes the pre-reservation-arming mutation produce +1 and fail:
	// the baseline is one Construction owner, and the refusal must leave it
	// exactly unchanged (no retained owner may appear at any point that
	// survives the refusal).
	pinTestConstruction(t, caps, "reg-refusal")
	owners0, con0 := caps.ownerCounts()
	require.Equal(t, 1, owners0)
	require.Equal(t, 1, con0)

	call := SessionAgentCall{
		SessionID:   sid,
		Prompt:      "refused",
		CallOptions: &CallOptions{},
		Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-refusal")},
	}

	// Run wins the in-process reservation, fails the lock, refuses.
	res, err := a.Run(context.Background(), call)
	require.Error(t, err)
	require.Nil(t, res)

	// FIRST PRODUCTION-FACING ASSERTION: zero retained owners leaked.
	owners1, con1 := caps.ownerCounts()
	require.Equal(t, owners0, owners1)
	require.Equal(t, con0, con1)
	// The reservation was abandoned, so the session is not busy.
	require.False(t, a.IsSessionBusy(sid))
}

// TestQueuedCallScope_RetentionSurvivesSenderScopeClose pins the arming
// lifetime directly on a scratch call: the armed owner keeps the
// construction alive after the sender's scope closes, the hook-free release
// via releaseQueuedBuildOwnership returns every count to zero, and a second
// release is a no-op.
func TestQueuedCallScope_RetentionSurvivesSenderScopeClose(t *testing.T) {
	t.Parallel()
	caps := NewDiskProviderRegistry()
	a := &sessionAgent{}
	a.bindDiskCaps(caps)

	// Simulate the sender's build: pin the construction into a scope the
	// sender will close on return.
	senderScope := newBuildResourceScope()
	// Fixture acquire (not production-facing): the sender-side construction
	// owner the armed scope must outlive.
	require.NoError(t, caps.acquireConstruction("", "reg-l1", "constr-l1"))
	senderScope.register(func() {
		caps.release(diskOwnerKey{kind: diskOwnerConstruction, instance: "constr-l1", registryID: "reg-l1"})
	})

	opts := &CallOptions{buildScope: senderScope}
	call := SessionAgentCall{
		SessionID:   "s-l1",
		Prompt:      "p",
		CallOptions: opts,
		Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-l1")},
	}

	owners0, con0 := caps.ownerCounts()
	require.Equal(t, 1, owners0)
	require.Equal(t, 1, con0)

	a.armQueuedBuildRetention(&call)

	// The call now carries its own copied options, the retained marker, and
	// NO hook (flaw B: onQueueResolved is never touched).
	require.NotNil(t, call.CallOptions)
	require.NotSame(t, opts, call.CallOptions)
	require.True(t, call.CallOptions.buildScopeRetained)
	require.Nil(t, call.onQueueResolved)
	// The arming acquire adds a SECOND Construction owner on the existing
	// record: owners +1, constructions unchanged.
	owners1, con1 := caps.ownerCounts()
	require.Equal(t, owners0+1, owners1)
	require.Equal(t, con0, con1)

	// The sender returns and its scope closes; the retained owner alone
	// keeps the record alive.
	senderScope.close()
	ownersMid, conMid := caps.ownerCounts()
	require.Equal(t, 1, ownersMid)
	require.Equal(t, 1, conMid)

	// The queue-exit release closes the retained scope: back to EMPTY.
	releaseQueuedBuildOwnership(&call)
	owners2, con2 := caps.ownerCounts()
	require.Equal(t, 0, owners2)
	require.Equal(t, 0, con2)

	// Calling the release again is exactly-once: still empty.
	releaseQueuedBuildOwnership(&call)
	owners3, con3 := caps.ownerCounts()
	require.Equal(t, 0, owners3)
	require.Equal(t, 0, con3)
}

// TestQueuedCallScope_NoRetentionGuards pins the OFF-mode boundary: every
// call shape that cannot outlive a sender's scope is left completely
// untouched — same options pointer, no hook, no owner keys — and an
// already-marked copy is never double-acquired.
func TestQueuedCallScope_NoRetentionGuards(t *testing.T) {
	t.Parallel()
	caps, a := newQueuedCallScopeFixture(t)
	// A separate agent with no bound capability manager (no-caps case).
	bare := &sessionAgent{}

	deferredTool := &batchIngressTool{bind: &batchCoordinatorBinding{}}

	cases := []struct {
		name string
		caps *DiskProviderRegistry
		ag   *sessionAgent
		pin  string // real construction pinned per guard case ("" = none)
		call SessionAgentCall
	}{
		{
			name: "drain",
			caps: caps, ag: a, pin: "reg-l4a",
			call: SessionAgentCall{
				SessionID: "s-drain", Prompt: "p", IsDrain: true,
				CallOptions: &CallOptions{},
				Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-l4a")},
			},
		},
		{
			name: "durable",
			caps: caps, ag: a, pin: "reg-l4b",
			call: SessionAgentCall{
				SessionID: "s-durable", Prompt: "p", FromDurableQueue: true,
				CallOptions: &CallOptions{},
				Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-l4b")},
			},
		},
		{
			name: "already hooked",
			caps: caps, ag: a, pin: "reg-l4c",
			call: SessionAgentCall{
				SessionID: "s-hooked", Prompt: "p",
				CallOptions:     &CallOptions{},
				Tools:           []fantasy.AgentTool{pinnedIngressTool("reg-l4c")},
				onQueueResolved: func(*fantasy.AgentResult, error) {},
			},
		},
		{
			name: "nil options",
			caps: caps, ag: a, pin: "reg-l4d",
			call: SessionAgentCall{
				SessionID: "s-nilopts", Prompt: "p",
				Tools: []fantasy.AgentTool{pinnedIngressTool("reg-l4d")},
			},
		},
		{
			name: "deferred wrapper only",
			caps: caps, ag: a,
			call: SessionAgentCall{
				SessionID: "s-deferred", Prompt: "p",
				CallOptions: &CallOptions{},
				Tools:       []fantasy.AgentTool{deferredTool},
			},
		},
		{
			name: "fail-fast",
			caps: caps, ag: a, pin: "reg-l4g",
			call: SessionAgentCall{
				SessionID: "s-failfast", Prompt: "p", FailIfSessionBusy: true,
				CallOptions: &CallOptions{},
				Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-l4g")},
			},
		},
		{
			name: "no caps",
			caps: nil, ag: bare,
			call: SessionAgentCall{
				SessionID: "s-nocaps", Prompt: "p",
				CallOptions: &CallOptions{},
				Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-l4f")},
			},
		},
	}

	// Register the per-case pins with the OUTER t (the table — and its
	// pins — is built before the subtests run): every pinned case's
	// baseline is the total pin count on the shared registry, and the arm
	// must leave it unchanged. The pin makes guard removal produce +1 and
	// fail. l4-deferred needs no registry (a deferred wrapper never
	// retains), and the no-caps case stays unpinned on the bare agent.
	pins := 0
	for _, tc := range cases {
		if tc.pin != "" {
			pinTestConstruction(t, caps, tc.pin)
			pins++
		}
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.call.CallOptions
			hooked := tc.call.onQueueResolved != nil
			if tc.caps != nil {
				owners0, con0 := tc.caps.ownerCounts()
				if tc.pin != "" {
					require.Equal(t, pins, owners0)
					require.Equal(t, pins, con0)
				}
				tc.ag.armQueuedBuildRetention(&tc.call)
				owners1, con1 := tc.caps.ownerCounts()
				require.Equal(t, owners0, owners1)
				require.Equal(t, con0, con1)
			} else {
				tc.ag.armQueuedBuildRetention(&tc.call)
			}
			if opts == nil {
				require.Nil(t, tc.call.CallOptions)
			} else {
				require.Same(t, opts, tc.call.CallOptions)
			}
			if hooked {
				require.NotNil(t, tc.call.onQueueResolved)
			} else {
				require.Nil(t, tc.call.onQueueResolved)
			}
		})
	}

	// Already-marked copy: arming an armed call again must not
	// double-acquire — owner count stays at exactly one retained +1.
	t.Run("already marked", func(t *testing.T) {
		t.Parallel()
		caps := NewDiskProviderRegistry()
		ag := &sessionAgent{}
		ag.bindDiskCaps(caps)
		// Fixture acquire (not production-facing): the construction record
		// the arming acquire needs in order to succeed at all.
		require.NotNil(t, pinTestConstruction(t, caps, "reg-l4m"))
		call := SessionAgentCall{
			SessionID: "s-marked", Prompt: "p",
			CallOptions: &CallOptions{},
			Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-l4m")},
		}
		ownersBase, _ := caps.ownerCounts()
		require.Equal(t, 1, ownersBase) // the pinTestConstruction owner only
		ag.armQueuedBuildRetention(&call)
		armed := call.CallOptions
		owners0, con0 := caps.ownerCounts()
		require.Equal(t, ownersBase+1, owners0)
		ag.armQueuedBuildRetention(&call)
		require.Same(t, armed, call.CallOptions)
		owners1, con1 := caps.ownerCounts()
		require.Equal(t, owners0, owners1)
		require.Equal(t, con0, con1)
	})
}

// TestQueuedCallScope_TurnEndReleaseRule is the direct unit coverage of
// releaseTurnEndBuildScope — the dispatcher's end-of-turn close rule
// (agent_run.go). MUTATION CAUGHT — reverting agent_run.go's rule to
// origin-inequality-only makes case (1) leak: a retained call re-run as a
// FRESH dispatcher's FIRST call (manual compaction) has origin == its own
// copy, so an inequality-only test would never close its scope.
func TestQueuedCallScope_TurnEndReleaseRule(t *testing.T) {
	t.Parallel()

	newMarkedCall := func(t *testing.T, ag *sessionAgent, sid, reg string) SessionAgentCall {
		t.Helper()
		pinTestConstruction(t, agCapsForTest(t, ag), reg)
		call := SessionAgentCall{
			SessionID:   sid,
			Prompt:      "p",
			CallOptions: &CallOptions{},
			Tools:       []fantasy.AgentTool{pinnedIngressTool(reg)},
		}
		ag.armQueuedBuildRetention(&call)
		require.True(t, call.CallOptions.buildScopeRetained)
		return call
	}

	t.Run("marked copy as its own origin", func(t *testing.T) {
		t.Parallel()
		caps, a := newQueuedCallScopeFixture(t)
		call := newMarkedCall(t, a, "s-ter1", "reg-ter1")
		owners0, con0 := caps.ownerCounts() // pin + retained
		require.Equal(t, 2, owners0)
		require.Equal(t, 1, con0)

		releaseTurnEndBuildScope(&call, call.CallOptions)
		require.False(t, call.CallOptions.buildScopeRetained)
		owners1, con1 := caps.ownerCounts()
		require.Equal(t, owners0-1, owners1)
		require.Equal(t, con0, con1)
	})

	t.Run("marked copy with a different origin", func(t *testing.T) {
		t.Parallel()
		caps, a := newQueuedCallScopeFixture(t)
		call := newMarkedCall(t, a, "s-ter2", "reg-ter2")
		origin := &CallOptions{}

		releaseTurnEndBuildScope(&call, origin)
		require.False(t, call.CallOptions.buildScopeRetained)
		_, con1 := caps.ownerCounts()
		require.Equal(t, 1, con1) // the pin only
	})

	t.Run("unmarked origin copy untouched", func(t *testing.T) {
		t.Parallel()
		caps, _ := newQueuedCallScopeFixture(t)
		pinTestConstruction(t, caps, "reg-ter3")
		call := SessionAgentCall{
			SessionID:   "s-ter3",
			Prompt:      "p",
			CallOptions: &CallOptions{},
			Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-ter3")},
		}
		owners0, _ := caps.ownerCounts()
		require.Equal(t, 1, owners0)

		releaseTurnEndBuildScope(&call, call.CallOptions)
		require.False(t, call.CallOptions.buildScopeRetained) // already false
		require.NotNil(t, call.CallOptions)                   // same options untouched
		owners1, _ := caps.ownerCounts()
		require.Equal(t, owners0, owners1) // untouched: the owner stays
	})

	t.Run("hooked call untouched", func(t *testing.T) {
		t.Parallel()
		caps, a := newQueuedCallScopeFixture(t)
		call := newMarkedCall(t, a, "s-ter4", "reg-ter4")
		call.onQueueResolved = func(*fantasy.AgentResult, error) {}
		owners0, _ := caps.ownerCounts()

		releaseTurnEndBuildScope(&call, &CallOptions{})
		require.True(t, call.CallOptions.buildScopeRetained) // untouched
		owners1, _ := caps.ownerCounts()
		require.Equal(t, owners0, owners1)
	})

	t.Run("second invocation is a no-op", func(t *testing.T) {
		t.Parallel()
		caps, a := newQueuedCallScopeFixture(t)
		call := newMarkedCall(t, a, "s-ter5", "reg-ter5")

		releaseTurnEndBuildScope(&call, call.CallOptions)
		owners1, _ := caps.ownerCounts()
		releaseTurnEndBuildScope(&call, call.CallOptions)
		require.False(t, call.CallOptions.buildScopeRetained)
		owners2, _ := caps.ownerCounts()
		require.Equal(t, owners1, owners2)
	})
}

// agCapsForTest returns the capability manager bound to a fixture agent
// (newQueuedCallScopeFixture always binds one).
func agCapsForTest(t *testing.T, ag *sessionAgent) *DiskProviderRegistry {
	t.Helper()
	caps := ag.diskCapsRef()
	require.NotNil(t, caps)
	return caps
}

// TestQueuedCallScope_FreshRunRefusalReleasesRetained pins invariant I1: a
// marked call that ENTERS Run as the compaction handoff's shape (it ARRIVES
// armed, agent_compaction.go:295 → a.Run) is released on the pre-turn
// refusal returns that happen before the dispatcher's end-of-turn close.
//
// MUTATION CAUGHT — removing the I1 defer at the top of Run leaves owners at
// 2 after the refusal.
func TestQueuedCallScope_FreshRunRefusalReleasesRetained(t *testing.T) {
	t.Parallel()

	t.Run("inter-process lock refused", func(t *testing.T) {
		t.Parallel()
		caps := NewDiskProviderRegistry()
		dataDir := t.TempDir()
		a := newLockTestSessionAgent(dataDir, false)
		a.bindDiskCaps(caps)
		sid := "s-freshrun-refusal"
		lk, err := session.TryAcquireSessionLockWithOptions(dataDir, sid) // held elsewhere
		require.NoError(t, err)
		t.Cleanup(func() { _ = lk.Release() })
		pinTestConstruction(t, caps, "reg-fresh")
		call := SessionAgentCall{SessionID: sid, Prompt: "p", CallOptions: &CallOptions{}, Tools: []fantasy.AgentTool{pinnedIngressTool("reg-fresh")}}
		a.armQueuedBuildRetention(&call) // the compaction handoff's shape: the call ARRIVES marked
		owners0, con0 := caps.ownerCounts()
		require.Equal(t, 2, owners0) // pin + retained
		require.Equal(t, 1, con0)
		res, err := a.Run(context.Background(), call)
		require.Error(t, err) // pre-turn lock refusal
		require.Nil(t, res)
		// I1: the retained owner is released on this pre-turn return.
		owners1, con1 := caps.ownerCounts()
		require.Equal(t, 1, owners1) // the pin only
		require.Equal(t, 1, con1)
	})

	t.Run("shutdown refused", func(t *testing.T) {
		t.Parallel()
		caps := NewDiskProviderRegistry()
		dataDir := t.TempDir()
		a := newLockTestSessionAgent(dataDir, false)
		a.bindDiskCaps(caps)
		sid := "s-freshrun-shutdown"
		// No lock held: the refusal must come from the shutdown branch
		// (tryAdmitRunWg → ErrAgentShuttingDown), which returns BEFORE the
		// dispatcher's end-of-turn release.
		a.shuttingDown.Store(true)
		pinTestConstruction(t, caps, "reg-fresh-shut")
		call := SessionAgentCall{SessionID: sid, Prompt: "p", CallOptions: &CallOptions{}, Tools: []fantasy.AgentTool{pinnedIngressTool("reg-fresh-shut")}}
		a.armQueuedBuildRetention(&call) // arrives marked
		owners0, con0 := caps.ownerCounts()
		require.Equal(t, 2, owners0) // pin + retained
		require.Equal(t, 1, con0)
		res, err := a.Run(context.Background(), call)
		require.ErrorIs(t, err, ErrAgentShuttingDown)
		require.Nil(t, res)
		// I1: the retained owner is released on this pre-turn return.
		owners1, con1 := caps.ownerCounts()
		require.Equal(t, 1, owners1) // the pin only
		require.Equal(t, 1, con1)
	})
}

// TestQueuedCallScope_SenderFirstRunUnaffected pins the sender-side boundary
// of the arming rule: the sender's OWN call parameter is never marked —
// arming happens on the COPY stored in mb.submitted — so the I1 entry defer
// in Run is a no-op for the sender's first Run.
//
// MUTATION CAUGHT — arming the sender's call in place (or double-arming)
// breaks require.Same / marker false.
func TestQueuedCallScope_SenderFirstRunUnaffected(t *testing.T) {
	t.Parallel()
	caps, a := newQueuedCallScopeFixture(t)
	sid := "s-sender"
	_, epoch, cancel, ok := a.ReserveExclusive(context.Background(), sid)
	require.True(t, ok)
	t.Cleanup(func() { a.ReleaseExclusive(sid, epoch, cancel) })
	pinTestConstruction(t, caps, "reg-sender")
	opts := &CallOptions{}
	call := SessionAgentCall{SessionID: sid, Prompt: "queued", CallOptions: opts, Tools: []fantasy.AgentTool{pinnedIngressTool("reg-sender")}}
	res, err := a.Run(context.Background(), call)
	require.NoError(t, err)
	require.Nil(t, res)
	// The SENDER's call parameter is untouched: arming happened on the copy in mb.submitted.
	require.Same(t, opts, call.CallOptions)
	require.False(t, call.CallOptions.buildScopeRetained)
	mb := a.getMailbox(sid)
	require.True(t, mb.submitted[0].CallOptions.buildScopeRetained)
	owners0, _ := caps.ownerCounts()
	require.Equal(t, 2, owners0) // pin + armed copy
	mb.clearAll()                // I2: queue-exit release
	owners1, con1 := caps.ownerCounts()
	require.Equal(t, 1, owners1)
	require.Equal(t, 1, con1)
}

// TestQueuedCallScope_DispatcherSiteFiresOnRealTurn drives the REAL
// dispatcher end-of-turn close site through a REAL completed turn on the
// mock model, with the turnEndRelease seam wrapped to count firings.
//
// The production call-site wiring is asserted by EXECUTION here: the seam
// only fires if runOwned's turn loop (agent_run.go) actually invokes
// turnEndRelease during a real Run. turnEndRelease is a package-level var:
// this test must stay serial (no t.Parallel) and restores it via
// t.Cleanup. Accepted CI caveat: parallel subtests of OTHER top-level tests
// in this package could still interleave with the swapped seam — the
// wrapper delegates to releaseTurnEndBuildScope, so behavior is unchanged
// for them; only the counter could over/under-count in that window.
//
// MUTATION CAUGHT — deleting the turnEndRelease call from runOwned's turn
// loop leaves fired at 0 and the marker true.
func TestQueuedCallScope_DispatcherSiteFiresOnRealTurn(t *testing.T) {
	env := testEnv(t)
	caps := NewDiskProviderRegistry()
	lm := &countingModel{}
	agentIface := NewSessionAgent(SessionAgentOptions{
		SmartModel:           Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		FastModel:            Model{Model: lm, CatwalkCfg: catwalk.Model{ContextWindow: 200000, DefaultMaxTokens: 10000}},
		SystemPrompt:         "test",
		IsYolo:               true,
		Sessions:             env.sessions,
		Messages:             env.messages,
		Tools:                []fantasy.AgentTool{},
		DisableAutoSummarize: true,
	})
	a := agentIface.(*sessionAgent)
	t.Cleanup(func() { a.CancelAll() })
	a.bindDiskCaps(caps)

	sess, err := env.sessions.Create(t.Context(), "dispatcher site")
	require.NoError(t, err)
	// A non-default title + a prior message keep needsTitle false, so
	// generateTitle's background calls never interleave with the turn this
	// test counts.
	require.NoError(t, env.sessions.Rename(t.Context(), sess.ID, "already titled"))
	sess, err = env.sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)

	// Pin BEFORE arming, so the arming acquire succeeds against the record.
	pinTestConstruction(t, caps, "reg-site")

	prev := turnEndRelease
	var fired int32
	turnEndRelease = func(call *SessionAgentCall, origin *CallOptions) {
		atomic.AddInt32(&fired, 1)
		releaseTurnEndBuildScope(call, origin)
	}
	t.Cleanup(func() { turnEndRelease = prev })

	call := SessionAgentCall{SessionID: sess.ID, Prompt: "turn", CallOptions: &CallOptions{}, Tools: []fantasy.AgentTool{pinnedIngressTool("reg-site")}}
	a.armQueuedBuildRetention(&call) // arrives marked = first-call shape
	owners0, con0 := caps.ownerCounts()
	require.Equal(t, 2, owners0) // pin + retained
	require.Equal(t, 1, con0)

	_, err = a.Run(t.Context(), call) // real turn on the mock model
	require.NoError(t, err)

	// The REAL dispatcher site fired.
	require.Positive(t, atomic.LoadInt32(&fired))
	// The marker was cleared by the site.
	require.False(t, call.CallOptions.buildScopeRetained)
	// Owner counts back to the pin baseline: the real site closed the
	// retained scope.
	owners1, con1 := caps.ownerCounts()
	require.Equal(t, 1, owners1) // the pin only
	require.Equal(t, 1, con1)
}
