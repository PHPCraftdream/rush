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

	"charm.land/fantasy"
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
// carrying ParentSessionID), whose parent claimed a matching JobKindAgent
// delegation row that has since gone terminal (no currently-RUNNING
// delegation row), must be REFUSED a Drain turn outright -- never falls
// through to the generic web/default policy that would otherwise grant it
// headroom.
//
// Revert-check performed: removed the isDurableDelegationChild branch from
// sessionDrainPolicy (coordinator_drain_policy.go), letting the "no running
// delegation" case fall straight through to the generic policy -- this
// test's `require.False(t, allowed)` FAILED (allowed was true, counted
// true: the child was granted the SAME auto-turn headroom as a plain web
// session). Restored the branch; re-ran, passed.
func TestSessionDrainPolicy_DurableDelegationChildWithNoRunningDelegation_Refused(t *testing.T) {
	coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	parent, err := env.sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(ctx, "task-call-1", parent.ID, "child")
	require.NoError(t, err)

	// item 8 fix: isDurableDelegationChild no longer trusts ParentSessionID
	// alone -- it confirms a real delegation job named this session as its
	// ChildSessionID. Claim + transition it to terminal so
	// hasRunningDelegationFor reports false (exactly like a released or
	// long-finished delegation) while the durable async_jobs row backing
	// the identity check still exists.
	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: parent.ID, ToolCallID: "agent-call-1", Kind: session.JobKindAgent,
		Input: "x", ChildSessionID: child.ID, ToolName: "agent",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, parent.ID, "agent-call-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: parent.ID, ToolCallID: "agent-call-1", State: "completed",
		ResultSummary: "done", Wake: true,
	})
	require.NoError(t, err)

	allowed, counted, err := coord.sessionDrainPolicy(ctx, child.ID)
	require.NoError(t, err)
	require.False(t, allowed, "a durable delegation child with no running delegation must be refused a Drain turn")
	require.False(t, counted)
}

// TestSessionDrainPolicy_ReleasedDelegationChildOfAnyKind_Refused: a durable
// delegation child is recognised by ANY of its parent's job rows naming it as
// child_session_id, not only Kind=agent -- a released agentic_fetch (fetch)
// child, or any other kind claiming a child session, must be refused a Drain
// turn just like a released agent child. A fork of the same parent, with no
// row naming it, stays on the ordinary policy.
//
// Revert-check: restored the `&& job.Kind == string(session.JobKindAgent)`
// clause in isDurableDelegationChild -- the fetch and command subtests FAILED
// (allowed was true); the agent subtest and the fork check stayed green.
func TestSessionDrainPolicy_ReleasedDelegationChildOfAnyKind_Refused(t *testing.T) {
	for _, kind := range []session.JobKind{session.JobKindAgent, session.JobKindFetch, session.JobKindCommand} {
		t.Run(string(kind), func(t *testing.T) {
			coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
			env := getEnv(context.Background())
			ctx := context.Background()

			parent, err := env.sessions.Create(ctx, "parent")
			require.NoError(t, err)
			child, err := env.sessions.CreateTaskSession(ctx, "task-call-1", parent.ID, "child")
			require.NoError(t, err)
			fork, _, err := env.sessions.ForkSessionTx(ctx, parent.ID, session.ForkOptions{
				NewID: "forked-child", ParentID: parent.ID,
			})
			require.NoError(t, err)

			_, err = store.Claim(ctx, session.ClaimParams{
				Owner: parent.ID, ToolCallID: "call-1", Kind: kind,
				Input: "x", ChildSessionID: child.ID, ToolName: string(kind),
			})
			require.NoError(t, err)
			require.NoError(t, store.MarkAnnounced(ctx, parent.ID, "call-1"))
			_, err = store.Transition(ctx, session.TransitionParams{
				Owner: parent.ID, ToolCallID: "call-1", State: "completed",
				ResultSummary: "done", Wake: true,
			})
			require.NoError(t, err)

			allowed, counted, err := coord.sessionDrainPolicy(ctx, child.ID)
			require.NoError(t, err)
			require.False(t, allowed, "a released %s delegation child must be refused a Drain turn", kind)
			require.False(t, counted)

			allowed, _, err = coord.sessionDrainPolicy(ctx, fork.ID)
			require.NoError(t, err)
			require.True(t, allowed, "a fork of the same parent (no row names it) stays on the ordinary policy")
		})
	}
}

// TestSessionDrainPolicy_ForkedChildWithParentSet_UsesWebPolicy is the item
// 8 fix's regression guard: `sessions fork --child` sets ParentSessionID for
// a purpose entirely unrelated to delegation (session.ForkOptions.ParentID)
// -- no async_jobs row ever names the fork as anyone's ChildSessionID. Such
// a session must NOT be swept into the delegation-child refusal.
//
// Revert-check performed: reverted isDurableDelegationChild to the bare
// `sess.ParentSessionID != ""` check (pre-item-8 behavior) -- this test's
// `require.True(t, allowed)` FAILED (allowed was false: the forked session
// was wrongly refused a Drain turn as if it were a released delegation
// child). Restored the ListAsyncJobsForOwner/ChildSessionID confirmation;
// re-ran, passed.
func TestSessionDrainPolicy_ForkedChildWithParentSet_UsesWebPolicy(t *testing.T) {
	coord, _, _, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()

	src, err := env.sessions.Create(ctx, "fork-source")
	require.NoError(t, err)
	fork, _, err := env.sessions.ForkSessionTx(ctx, src.ID, session.ForkOptions{
		NewID: "forked-child", ParentID: src.ID,
	})
	require.NoError(t, err)
	require.Equal(t, src.ID, fork.ParentSessionID, "fixture sanity: fork must carry ParentSessionID like sessions fork --child")

	allowed, _, err := coord.sessionDrainPolicy(ctx, fork.ID)
	require.NoError(t, err)
	require.True(t, allowed, "a forked (non-delegation) session with ParentSessionID set must use the ordinary web/default policy, not the delegation-child refusal")
}

// panicIfCalledAgent is a SessionAgent whose Run panics -- used to prove a
// caller NEVER reaches it at all, stronger than a counter a test might
// forget to assert on.
type panicIfCalledAgent struct{ *mockSessionAgent }

func (a *panicIfCalledAgent) Run(context.Context, SessionAgentCall) (*fantasy.AgentResult, error) {
	panic("released delegation child must never route a Drain turn to the root coder agent")
}

// TestWakeSession_ReleasedDelegationChild_EndToEnd_NeverReachesRootAgent is
// W-DRAIN item C's end-to-end version of TestSessionDrainPolicy_
// DurableDelegationChildWithNoRunningDelegation_Refused above: that test
// calls sessionDrainPolicy directly; this drives the exact same scenario
// through the REAL wakeSession entry point (coordinator_wake.go) -- the
// thing an actual completion/hint calls -- with NO driver registered for the
// child (releaseDriverIfScopeClosed's real effect once scope closes) and
// c.currentAgent set to an agent that PANICS if Run is ever called, proving
// the security-relevant danger the review names directly: a released child
// falling through to agentFor's currentAgent fallback (the ROOT coder agent:
// full tool set, no RunAllowlist, no child system prompt) never happens --
// sessionDrainPolicy's refusal short-circuits wakeSession before agentFor is
// ever consulted at all.
//
// Revert-check performed: removed the isDurableDelegationChild branch from
// sessionDrainPolicy (coordinator_drain_policy.go), same as the unit-level
// test's own revert-check -- this test PANICKED (panicIfCalledAgent.Run was
// actually invoked: wakeSession fell through to the generic web/default
// policy, which allowed the turn, which then reached agentFor's currentAgent
// fallback exactly as the finding describes). Restored the branch; re-ran,
// passed (no panic, err == nil).
func TestWakeSession_ReleasedDelegationChild_EndToEnd_NeverReachesRootAgent(t *testing.T) {
	coord, _, store, getEnv := newChildPolicyTestCoordinator(t)
	env := getEnv(context.Background())
	ctx := context.Background()
	coord.currentAgent = &panicIfCalledAgent{mockSessionAgent: &mockSessionAgent{}}

	parent, err := env.sessions.Create(ctx, "parent-e2e")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(ctx, "task-call-1", parent.ID, "child-e2e")
	require.NoError(t, err)

	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: parent.ID, ToolCallID: "agent-call-1", Kind: session.JobKindAgent,
		Input: "x", ChildSessionID: child.ID, ToolName: "agent",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, parent.ID, "agent-call-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: parent.ID, ToolCallID: "agent-call-1", State: "completed",
		ResultSummary: "done", Wake: true,
	})
	require.NoError(t, err)

	// The child's OWN debt (e.g. a stray notice/timeout that fired after its
	// delegation released) -- exactly what would otherwise force a Drain
	// turn if the policy refusal did not short-circuit first. No driver is
	// registered for child.ID at all (releaseDriverIfScopeClosed's real
	// post-release state), so agentFor(child.ID) would return
	// c.currentAgent if wakeSession ever reached that far.
	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: child.ID, ToolCallID: "call-x", Kind: session.JobKindCommand, Input: "x", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, child.ID, "call-x"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: child.ID, ToolCallID: "call-x", State: "completed", ResultSummary: "output", Wake: true,
	})
	require.NoError(t, err)
	_, err = store.PullJobNotices(ctx, env.messages, child.ID, buildJobNoticeMessageParams)
	require.NoError(t, err)

	err = coord.wakeSession(ctx, jobIdentity{owner: child.ID, toolCallID: "call-x"}, true)
	require.NoError(t, err, "a policy-refused Drain is a silent no-op, never an error")
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

// TestChildScopeDrained_TerminalRowWithUnreactedDebt_NotDrained pins the
// CORRECTED B3/C6 design: childScopeOpenAcrossProcesses uses coordinator.
// ScopeOpen's FULL predicate (a cross-process running row, OR outstanding
// reaction debt), not running-rows alone.
//
// History: an earlier pass narrowed this check to running-rows-only,
// because folding in the debt half made every pre-existing bare-ledger
// delegation test in work_ledger_delegation_test.go fail -- those tests'
// synthetic jobs finish with wake=1/delivery=pending/done and nothing in a
// bare-ledger unit test ever drives a real reacting turn, so the debt never
// cleared and no delegation could release. That narrowing was ITSELF the
// bug, not a deliberate scope choice: it reopened exactly the race B3/C6 set
// out to close, provably so via
// TestRunNonInteractiveChildReceivesAsyncBashResultFinishedMidTurn
// (internal/app) -- an end-to-end test that needs the delegation to stay
// parked until the child's OWN async job result has actually been reacted
// to, not merely finished. The fix restores the full ScopeOpen predicate in
// production code and instead corrects the bare-ledger tests to simulate
// the child's reaction explicitly (work_ledger_delegation_test.go's
// simulateChildReaction helper) before asserting a release.
//
// Revert-check performed: reverted childScopeOpenAcrossProcesses
// (work_ledger_delegation.go) to the running-rows-only check -- this test's
// `require.False(t, ledger.childScopeDrained(...))` FAILED (returned true).
// Restored the full ScopeOpen call; re-ran, passed.
func TestChildScopeDrained_TerminalRowWithUnreactedDebt_NotDrained(t *testing.T) {
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

	require.False(t, ledger.childScopeDrained(child.ID),
		"a pulled-but-unreacted notice is still outstanding reaction debt -- ScopeOpen must report the child's scope as still open")
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
