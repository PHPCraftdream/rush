package agent

// Package L round 2 queue-exit coverage: the orphan durable-re-enqueue
// handoff (restartOrphanedWithRetry), clearAll, the replacement-supersede
// safety net, the residual-shutdown sweep, and the per-call restricted-run
// policy carry through the real queueing path.

import (
	"context"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestQueuedCallScope_OrphanHandoffReenqueuesAndReleases drives the orphan
// exit: an armed queued call popped via the real abandonOwnershipAndPopSubmitted
// is durably re-enqueued by the real restartOrphanedWithRetry AND its
// in-process retained owner is released exactly once. The durable proof is
// the session_run_queue ROW itself (single-row QueryRowContext), not the
// recording fake; the recording fake only proves the handoff routed through
// the enqueue path.
//
// MUTATION CAUGHT — (1) reusing onQueueResolved for cleanup (round-1 flaw
// B): the armed call would carry a hook, splitCallsWithoutWaiters would
// resolve-and-drop it, no row would be enqueued, so the enqueue assertion
// fails; (2) removing the release loop in restartOrphanedWithRetry: the
// owner count stays +1 after the handoff.
func TestQueuedCallScope_OrphanHandoffReenqueuesAndReleases(t *testing.T) {
	t.Parallel()
	caps := NewDiskProviderRegistry()
	a := &sessionAgent{}
	a.bindDiskCaps(caps)
	a.mailboxes = csync.NewMap[string, *mailbox]()

	// A real session service over its own database, wrapped in the
	// enqueue-recording fake: the durable row goes to a REAL table, the
	// recording asserts only that the enqueue path was taken.
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseConn(conn) })
	svc := session.NewService(db.New(conn), conn)
	sess, err := svc.Create(t.Context(), "orphan-handoff")
	require.NoError(t, err)
	require.NotEmpty(t, sess.ID)
	sid := sess.ID
	rec := &enqueueRecordingSessions{Service: svc}
	a.sessions = rec
	_, epoch, cancel, ok := a.ReserveExclusive(context.Background(), sid)
	require.True(t, ok)
	t.Cleanup(func() { a.ReleaseExclusive(sid, epoch, cancel) })

	// pinTestConstruction's test-wide owner keeps the record alive, so this
	// test asserts the OWNER-COUNT delta, not record pruning.
	pinTestConstruction(t, caps, "reg-o1")

	call := SessionAgentCall{
		SessionID:     sess.ID,
		Prompt:        "orphaned",
		LogicalCallID: "lc-orphan-marker",
		CallOptions:   &CallOptions{},
		Tools:         []fantasy.AgentTool{pinnedIngressTool("reg-o1")},
	}

	owners0, con0 := caps.ownerCounts()
	require.Equal(t, 1, owners0)
	require.Equal(t, 1, con0)

	res, err := a.Run(context.Background(), call)
	require.NoError(t, err)
	require.Nil(t, res)

	mb := a.getMailbox(sid)
	require.Len(t, mb.submitted, 1)
	require.Equal(t, "lc-orphan-marker", mb.submitted[0].LogicalCallID)
	require.True(t, mb.submitted[0].CallOptions.buildScopeRetained)
	owners1, _ := caps.ownerCounts()
	require.Equal(t, owners0+1, owners1)

	popped := mb.abandonOwnershipAndPopSubmitted(epoch)
	require.Len(t, popped, 1)

	// The real orphan handoff: durable re-enqueue (synchronous) + the
	// round-2 release point. It waits internally for its enqueue
	// goroutines, so nothing here is left unjoined.
	require.NoError(t, a.restartOrphanedWithRetry(popped))

	// FIRST PRODUCTION-FACING ASSERTION: exactly one DURABLE row carrying
	// the marker LogicalCallID exists (session_run_queue.call_data, FKed on
	// the real session ID) — not just the recording fake's view of it.
	var callData string
	require.NoError(t, conn.QueryRowContext(t.Context(),
		`SELECT call_data FROM session_run_queue WHERE session_id = ?`, sess.ID).Scan(&callData))
	require.Contains(t, callData, "lc-orphan-marker")

	// Demoted assertion: the recording fake saw exactly one attempt — this
	// proves the handoff ROUTED through the enqueue path (the durable row
	// above is the authoritative proof).
	rec.mu.Lock()
	enqueued := len(rec.calls)
	rec.mu.Unlock()
	require.Equal(t, 1, enqueued)

	// The release fired once at the handoff: back to the pin-only baseline.
	owners2, con2 := caps.ownerCounts()
	require.Equal(t, owners0, owners2)
	require.Equal(t, con0, con2)

	// Exactly once: a second release stays at the baseline (the real
	// service's idempotency key would refuse a duplicate row, so the
	// double-restart variant is not exercised).
	releaseQueuedBuildOwnership(&popped[0])
	owners3, con3 := caps.ownerCounts()
	require.Equal(t, owners0, owners3)
	require.Equal(t, con0, con3)
}

// TestQueuedCallScope_ClearAndShutdownReleaseExactlyOnce covers the three
// remaining exit paths: clearAll, the replacement-supersede safety net, and
// the residual-shutdown sweep.
//
// MUTATION CAUGHT — removing the release in clearAll, or a non-idempotent
// double release.
func TestQueuedCallScope_ClearAndShutdownReleaseExactlyOnce(t *testing.T) {
	t.Parallel()
	t.Run("clearAll", func(t *testing.T) {
		t.Parallel()
		caps, a := newQueuedCallScopeFixture(t)
		sid := "s-clear"
		_, epoch, cancel, ok := a.ReserveExclusive(context.Background(), sid)
		require.True(t, ok)
		t.Cleanup(func() { a.ReleaseExclusive(sid, epoch, cancel) })

		pinTestConstruction(t, caps, "reg-clear")
		call := SessionAgentCall{
			SessionID:   sid,
			Prompt:      "queued",
			CallOptions: &CallOptions{},
			Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-clear")},
		}
		owners0, con0 := caps.ownerCounts()
		require.Equal(t, 1, owners0)

		mb := a.getMailbox(sid)
		mb.armQueuedBuild(&call)
		mb.queue(call)
		owners1, _ := caps.ownerCounts()
		require.Equal(t, owners0+1, owners1)

		mb.clearAll()
		owners2, con2 := caps.ownerCounts()
		require.Equal(t, owners0, owners2)
		require.Equal(t, con0, con2)

		// Second clearAll (empty queue): still the baseline.
		mb.clearAll()
		owners3, con3 := caps.ownerCounts()
		require.Equal(t, owners0, owners3)
		require.Equal(t, con0, con3)
	})

	t.Run("replacement supersede safety net", func(t *testing.T) {
		t.Parallel()
		caps, a := newQueuedCallScopeFixture(t)
		sid := "s-supersede"
		_, epoch, cancel, ok := a.ReserveExclusive(context.Background(), sid)
		require.True(t, ok)
		t.Cleanup(func() { a.ReleaseExclusive(sid, epoch, cancel) })

		pinTestConstruction(t, caps, "reg-super")
		owners0, con0 := caps.ownerCounts()
		require.Equal(t, 1, owners0)

		mb := a.getMailbox(sid)
		// Registered AFTER ReleaseExclusive's cleanup, so it runs BEFORE it
		// (LIFO) even when an assertion below fails: the installed
		// replacement is resolved first and never reaches orphan recovery
		// with a nil session service.
		t.Cleanup(func() { mb.clearAll() })
		retained := SessionAgentCall{
			SessionID:   sid,
			Prompt:      "replacement",
			CallOptions: &CallOptions{},
			Tools:       []fantasy.AgentTool{pinnedIngressTool("reg-super")},
		}
		mb.armQueuedBuild(&retained)
		require.True(t, retained.CallOptions.buildScopeRetained)
		mb.mu.Lock()
		mb.replacement = &retained
		mb.mu.Unlock()
		owners1, _ := caps.ownerCounts()
		require.Equal(t, owners0+1, owners1)

		other := SessionAgentCall{SessionID: sid, Prompt: "instead"}
		_, replaced := mb.interruptAndReplace(other)
		require.True(t, replaced)

		// The safety net released the old replacement's retained scope.
		owners2, con2 := caps.ownerCounts()
		require.Equal(t, owners0, owners2)
		require.Equal(t, con0, con2)

		// Resolve the installed replacement through the same exit path
		// BEFORE cleanup: otherwise the cleanup ReleaseExclusive pops the
		// unmarked, non-waiter, non-Drain "instead" call into
		// restartOrphanedWithRetry with a nil session service, whose
		// enqueue goroutine panics (agent_ownership.go:564).
		mb.clearAll()
	})

	t.Run("shutdown hardStop routing", func(t *testing.T) {
		t.Parallel()
		caps := NewDiskProviderRegistry()
		a := &sessionAgent{}
		a.bindDiskCaps(caps)
		a.mailboxes = csync.NewMap[string, *mailbox]()

		// Real db + fake-recording sessions: the queued call must reach the
		// DURABLE session_run_queue table via restartOrphanedWithRetry —
		// a.sessions must be non-nil for that enqueue to happen.
		dataDir := t.TempDir()
		conn, err := db.Connect(t.Context(), dataDir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.ReleaseConn(conn) })
		svc := session.NewService(db.New(conn), conn)
		sess, err := svc.Create(t.Context(), "shutdown-handoff")
		require.NoError(t, err)
		require.NotEmpty(t, sess.ID)
		a.sessions = &enqueueRecordingSessions{Service: svc}

		// Queue the armed call via the REAL Run path: ReserveExclusive
		// holds the epoch, so Run acknowledges the call as queued and arms
		// its retained build scope through mailbox.submit.
		_, epoch, cancel, ok := a.ReserveExclusive(context.Background(), sess.ID)
		require.True(t, ok)
		t.Cleanup(func() { a.ReleaseExclusive(sess.ID, epoch, cancel) })

		pinTestConstruction(t, caps, "reg-shut")
		call := SessionAgentCall{
			SessionID:     sess.ID,
			Prompt:        "queued",
			LogicalCallID: "lc-shutdown-marker",
			CallOptions:   &CallOptions{},
			Tools:         []fantasy.AgentTool{pinnedIngressTool("reg-shut")},
		}
		res, err := a.Run(context.Background(), call)
		require.NoError(t, err)
		require.Nil(t, res)

		mb := a.getMailbox(sess.ID)
		require.Len(t, mb.submitted, 1)
		require.True(t, mb.submitted[0].CallOptions.buildScopeRetained)
		owners0, con0 := caps.ownerCounts()
		require.Equal(t, 2, owners0) // pin + retained
		require.Equal(t, 1, con0)

		// What App teardown does: hardStop latches stopped, then the real
		// shutdown drain pops the queued call as orphaned (drainOrReleaseFinal:
		// stopped entry check skips the live branches -> mbReleasing with
		// release==nil -> the finalize step hands submitted out as orphaned;
		// the epoch must match, which it does here — hardStop never bumps it).
		mb.hardStop()
		_, _, orphaned, relErr := mb.drainOrReleaseFinal(epoch, nil)
		require.NoError(t, relErr)
		require.Len(t, orphaned, 1)
		require.Equal(t, "lc-shutdown-marker", orphaned[0].LogicalCallID)

		// The SAME handoff App/Run's exit path uses: durable re-enqueue +
		// the round-2 release point.
		require.NoError(t, a.restartOrphanedWithRetry(orphaned))

		// Release fired at the handoff: back to the pinned baseline —
		// asserted BEFORE any releaseResidualConstructions call.
		owners1, con1 := caps.ownerCounts()
		require.Equal(t, 1, owners1)
		require.Equal(t, 1, con1)

		// The durable proof: the row, not the recording fake.
		var callData string
		require.NoError(t, conn.QueryRowContext(t.Context(),
			`SELECT call_data FROM session_run_queue WHERE session_id = ?`, sess.ID).Scan(&callData))
		require.Contains(t, callData, "lc-shutdown-marker")

		// Only after the baseline assertions: the residual sweep deletes the
		// Construction owner keys underneath (dedicated registry, one
		// construction -> everything drops to empty), and the release stays
		// a no-op delete.
		caps.releaseResidualConstructions()
		owners2, con2 := caps.ownerCounts()
		require.Equal(t, 0, owners2)
		require.Equal(t, 0, con2)

		mb.clearAll()
		owners3, con3 := caps.ownerCounts()
		require.Equal(t, 0, owners3)
		require.Equal(t, 0, con3)

		mb.clearAll()
		owners4, con4 := caps.ownerCounts()
		require.Equal(t, 0, owners4)
		require.Equal(t, 0, con4)
	})
}

// scopePolicyProbe is the RECORDING inner implementation for the policy
// dispatch sequence (own small probe type: no coupling to the batch
// dispatch fixtures). The names "view" and "edit" classify under
// restrictedToolActions as "read" and "write".
type scopePolicyProbe struct {
	name   string
	called bool
}

func (p *scopePolicyProbe) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: p.name}
}

func (p *scopePolicyProbe) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	p.called = true
	return fantasy.NewTextResponse("dispatched"), nil
}

func (p *scopePolicyProbe) ProviderOptions() fantasy.ProviderOptions   { return nil }
func (p *scopePolicyProbe) SetProviderOptions(fantasy.ProviderOptions) {}

// TestQueuedCallScope_FourPolicyDecisionsReduced exercises the production
// restricted-run gate and the per-call policy carry. The app-level epoch
// orchestration (owner grant/deny interleaving) is NOT exercised here — it
// remains internal/app's
// TestExecuteRunLegacyQueueingTurnsRunUnderTheirOwnPolicies, unchanged.
// Likewise, the full per-call permission arming inside runOwned
// (SetSessionRunAllowlistForCall) is NOT exercised — that is the app test's
// job. What is proven here: the restricted gate's four decisions on bare
// probes, and that a RETAINED queued call's carried policy pointer is
// intact and usable after promotion through the real drainOrReleaseFinal
// pop.
//
// MUTATION CAUGHT — (i) a retention copy that dropped RunAllowlist breaks
// require.Same; (ii) arming/judging under the wrong policy breaks the
// allow/deny outcomes; (iii) the fresh-probe reset makes a stale `called`
// flag impossible to mask a denial.
func TestQueuedCallScope_FourPolicyDecisionsReduced(t *testing.T) {
	t.Parallel()

	newAllow := func(t *testing.T, allowTools ...string) permission.RunAllowlist {
		t.Helper()
		allowlist, err := permission.BuildRunAllowlist(permission.RunAllowlistSpec{
			Restrict:   true,
			AllowTools: allowTools,
		})
		require.NoError(t, err)
		return allowlist
	}
	// dispatch builds a FRESH probe per dispatch (a stale `called` flag
	// could otherwise mask a denial), arms the production restricted-run
	// wrapper with the given allowlist, and runs the named tool.
	dispatch := func(t *testing.T, allowlist permission.RunAllowlist, name string) (granted, innerRan bool) {
		t.Helper()
		probe := &scopePolicyProbe{name: name}
		svc := permission.NewPermissionService(t.Context(), t.TempDir(), false, nil, nil)
		svc.(permission.SessionRunAllowlistManager).SetSessionRunAllowlist("session", allowlist)
		restricted := wrapToolsWithRestrictedRun([]fantasy.AgentTool{probe}, svc.(permission.RestrictedRunAuthorizer))[0]
		resp, err := restricted.Run(
			context.WithValue(t.Context(), tools.SessionIDContextKey, "session"),
			fantasy.ToolCall{ID: "probe-" + name, Name: name},
		)
		require.NoError(t, err) // denial returns err==nil + an IsError response
		return !resp.IsError, probe.called
	}

	policyA := newAllow(t, "view") // view:read
	policyB := newAllow(t, "edit") // edit:write

	// Four decisions: A allows "view"; A denies "edit"; B allows "edit";
	// B denies "view" (restrictedToolActions maps view→read, edit→write;
	// bare-name match, runallowlist.go).
	got, ran := dispatch(t, policyA, "view")
	require.True(t, got, "A allows view")
	require.True(t, ran)
	got, ran = dispatch(t, policyA, "edit")
	require.False(t, got, "A denies edit")
	require.False(t, ran)
	got, ran = dispatch(t, policyB, "edit")
	require.True(t, got, "B allows edit")
	require.True(t, ran)
	got, ran = dispatch(t, policyB, "view")
	require.False(t, got, "B denies view")
	require.False(t, ran)

	// The carry assertion: a queued call's own policy survives queueing
	// through the real Run path untouched — the SAME pointer — and the
	// RETAINED call's carried policy is intact and usable after promotion.
	caps, a := newQueuedCallScopeFixture(t)
	sid := "s-policy"
	_, epoch, cancel, ok := a.ReserveExclusive(context.Background(), sid)
	require.True(t, ok)
	t.Cleanup(func() { a.ReleaseExclusive(sid, epoch, cancel) })

	pinTestConstruction(t, caps, "reg-policy")
	call := SessionAgentCall{
		SessionID:     sid,
		Prompt:        "queued",
		LogicalCallID: "lc-policy",
		RunAllowlist:  &policyB,
		CallOptions:   &CallOptions{},
		Tools:         []fantasy.AgentTool{pinnedIngressTool("reg-policy")},
	}
	res, err := a.Run(context.Background(), call)
	require.NoError(t, err)
	require.Nil(t, res)

	mb := a.getMailbox(sid)
	require.Len(t, mb.submitted, 1)
	require.True(t, mb.submitted[0].CallOptions.buildScopeRetained)
	require.Same(t, &policyB, mb.submitted[0].RunAllowlist)

	// The REAL promotion pop (the primitive every live drain path uses via
	// drainOrReleaseMerged).
	next, hasNext, orphaned, relErr := mb.drainOrReleaseFinal(epoch, nil)
	require.True(t, hasNext)
	require.Len(t, orphaned, 0)
	require.NoError(t, relErr)
	require.True(t, next.CallOptions.buildScopeRetained)

	// Dispatch an "edit" probe under the RETAINED call's own policy —
	// granted (B allows edit).
	got, ran = dispatch(t, *next.RunAllowlist, "edit")
	require.True(t, got, "retained call's own policy allows edit")
	require.True(t, ran)
	// Under policyA — denied.
	got, ran = dispatch(t, policyA, "edit")
	require.False(t, got, "policyA denies edit")
	require.False(t, ran)
}
