// B3/C6 (docs/reviews/2026-09-29-async-phase4-round1.md, W-DRAIN): a
// released or expired delegation child must never get further Drain turns
// on the root coder agent. Two mechanisms, tested independently:
//
//  1. sessionDrainPolicy keys the child refusal on DURABLE identity
//     (ParentSessionID, set at creation by CreateTaskSession) instead of
//     falling through to the generic web/default policy once no RUNNING
//     delegation row claims the session -- the in-memory subAgentDrivers
//     registry is torn down the instant scope closes and cannot be used for
//     this decision.
//  2. childScopeDrained also considers cross-process state (a running async
//     job row on a live OTHER host, or outstanding DB reaction debt) that
//     this process's own in-memory workLedger/IsSessionBusy checks cannot
//     see, so a delegation is never released while the child's scope is
//     genuinely still open somewhere.
package agent

import (
	"context"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func newChildPolicyTestCoordinator(t *testing.T) (*coordinator, *workLedger, *session.AsyncJobStore, func(context.Context) fakeEnv) {
	t.Helper()
	env := testEnv(t)
	store := session.NewAsyncJobStore(env.conn, env.workingDir, os.Getpid(), "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	coord := &coordinator{sessions: env.sessions, messages: env.messages, subAgentDrivers: newSubAgentDriverRegistry()}
	ledger := newWorkLedger(coord.notifyAsyncCompletion)
	ledger.store = store
	ledger.coord = coord
	coord.asyncJobs = ledger
	return coord, ledger, store, func(context.Context) fakeEnv { return env }
}

// TestSessionDrainPolicy_DurableDelegationChildWithNoRunningDelegation_Refused
// is the core B3/C6 proof: a session created via CreateTaskSession (durably
// carrying ParentSessionID) with no currently-RUNNING delegation row must be
// REFUSED a Drain turn outright -- never falls through to the generic
// web/default policy that would otherwise grant it headroom.
//
// Revert-check performed: removed the isDurableDelegationChild branch from
// sessionDrainPolicy (coordinator_drain_policy.go), letting the "no running
// delegation" case fall straight through to the generic policy -- this
// test's `require.False(t, allowed)` FAILED (allowed was true, counted
// true: the child was granted the SAME auto-turn headroom as a plain web
// session). Restored the branch; re-ran, passed.
func TestSessionDrainPolicy_DurableDelegationChildWithNoRunningDelegation_Refused(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	parent, err := env.sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(ctx, "task-call-1", parent.ID, "child")
	require.NoError(t, err)

	// No delegation row was ever claimed for child.ID in the async_jobs
	// store -- hasRunningDelegationFor reports false, exactly like a
	// released or long-finished delegation.
	allowed, counted, err := coord.sessionDrainPolicy(ctx, child.ID)
	require.NoError(t, err)
	require.False(t, allowed, "a durable delegation child with no running delegation must be refused a Drain turn")
	require.False(t, counted)
}

// TestSessionDrainPolicy_PlainSessionWithNoDelegationHistory_UsesWebPolicy is
// the regression guard: a session that was NEVER a delegation target (no
// ParentSessionID) must keep following the ordinary web/default policy, not
// be swept up by the new child-identity check. It must still be ALLOWED a
// turn -- counted is false here per the separate B7 design decision (an
// ordinary async-job/delegation wake is uncapped, see
// coordinator_drain_cap_test.go), not because of the child-identity check
// this test targets.
func TestSessionDrainPolicy_PlainSessionWithNoDelegationHistory_UsesWebPolicy(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	sess, err := env.sessions.Create(ctx, "plain-web-session")
	require.NoError(t, err)

	allowed, _, err := coord.sessionDrainPolicy(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, allowed, "a plain session with no delegation history must use the web/default policy")
}

// TestChildScopeDrained_RunningRowOnLiveOtherHost_NotDrained pins
// childScopeOpenAcrossProcesses: a running async_jobs row for the child,
// with nothing at all in THIS process's own in-memory workLedger map (it was
// claimed directly through the store, bypassing ledger.Start, simulating a
// row claimed by a different process), must still keep the scope open.
//
// Revert-check performed: made childScopeDrained return true unconditionally
// once the in-memory/IsSessionBusy checks pass (removed the
// childScopeOpenAcrossProcesses call) -- this test's
// `require.False(t, ledger.childScopeDrained(...))` FAILED (childScopeDrained
// returned true despite the running row). Restored the call; re-ran, passed.
func TestChildScopeDrained_RunningRowOnLiveOtherHost_NotDrained(t *testing.T) {
	_, ledger, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "child-cross-process")
	require.NoError(t, err)

	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: child.ID, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)

	require.False(t, ledger.childScopeDrained(child.ID),
		"a running row on a live host must keep the child's scope open even with nothing in this process's own memory")
}

// TestChildScopeDrained_TerminalRowWithUnreactedDebt_StillDrained is a
// deliberate scope-narrowing regression guard, not a gap re-opened by
// accident: childScopeOpenAcrossProcesses checks ONLY cross-process running
// rows, not reaction debt. An earlier version of the B3/C6 fix folded
// ScopeOpen's full predicate (running-row OR debt) into childScopeDrained,
// which made EVERY pre-existing bare-ledger delegation test in
// work_ledger_delegation_test.go hang forever: those tests' synthetic jobs
// finish with wake=1/delivery=pending and are never pulled/reacted (nothing
// drives the child's own turn in a bare-ledger unit test), so the debt half
// never clears and no delegation could ever release. This codebase's
// existing delegation-delivery design deliberately treats "child owes a
// reaction" as the CHILD's own concern (its own wakeSession/Drain schedule),
// independent of whether its delegation to the parent may release -- see
// work_ledger_delegation_test.go's five pinned properties and
// finishChildJob's own doc. Known limitation carried forward from the
// original finding: a delegation CAN still release while its child has
// otherwise-unreacted debt sitting in the DB; closing that gap needs a
// design decision this wave did not make (see final report).
func TestChildScopeDrained_TerminalRowWithUnreactedDebt_StillDrained(t *testing.T) {
	_, ledger, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	child, err := env.sessions.Create(ctx, "child-debt")
	require.NoError(t, err)

	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: child.ID, ToolCallID: "call-1", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, child.ID, "call-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: child.ID, ToolCallID: "call-1", State: "completed", ResultSummary: "output", Wake: true,
	})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, env.messages, child.ID, buildJobNoticeMessageParams)
	require.NoError(t, err)

	require.True(t, ledger.childScopeDrained(child.ID),
		"childScopeDrained checks cross-process RUNNING rows only, never reaction debt (deliberate scope, see doc)")
}

// TestChildScopeDrained_NothingPending_Drained is the baseline: a child with
// no jobs, no debt, and no busy driver reports drained.
func TestChildScopeDrained_NothingPending_Drained(t *testing.T) {
	_, ledger, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()
	child, err := env.sessions.Create(ctx, "child-empty")
	require.NoError(t, err)
	require.True(t, ledger.childScopeDrained(child.ID))
}
