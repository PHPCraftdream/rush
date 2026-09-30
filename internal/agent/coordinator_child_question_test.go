// A delegated child's Drain turn (R2B-1 B, child half): a question the child
// asks while reacting to its own notice reaches the parent through the
// delegation's release, and debt the policy will never let the child act on
// (deferred: a question, Stop) does not hold the delegation open.
package agent

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// childDelegationFixture is a parent session delegating to a child session
// whose driver is a real *sessionAgent; deliveries to the parent land on ch.
func childDelegationFixture(t *testing.T, o attemptFixtureOpts) (*attemptFixture, string, string, chan AsyncCompletion) {
	t.Helper()
	ctx := context.Background()
	f := newAttemptFixture(t, "child-question-root", o)
	f.coord.messages = f.env.messages
	f.coord.sessions = f.env.sessions
	ch := make(chan AsyncCompletion, 4)
	f.ledger.onWebDone = func(c AsyncCompletion) { ch <- c }

	parent, err := f.env.sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := f.env.sessions.CreateTaskSession(ctx, "task-q", parent.ID, "child")
	require.NoError(t, err)
	f.coord.subAgentDrivers.register(child.ID, subAgentDriver{
		agent: f.sa, call: SessionAgentCall{SessionID: child.ID}, parentSessionID: parent.ID,
	})
	_, _, err = f.ledger.Start(parent.ID, "delegate-1", "do work", AgentToolName, child.ID, false, false, nil, func() {})
	require.NoError(t, err)
	f.ledger.acknowledged(jobOf(f.ledger, parent.ID, "delegate-1"))
	// The child owes a reaction to its own notice when its first turn ends.
	require.NoError(t, f.store.InsertSessionNotice(ctx, child.ID, "manual_test_notice", "child job finished", true, ""))
	return f, parent.ID, child.ID, ch
}

// TestChildDrainQuestion_ReachesParent: the child asks a question in its Drain
// turn; the question is the reaction (debt closed), automatic turns suspend,
// the delegation releases, and the parent gets the question -- as a paused
// sub-agent it can resume, not as a failure and not as "no final text".
//
// Revert-check: dropping subAgentQuestionFromFinish from
// refreshSubAgentCompletion delivers the "no final text" wording instead and
// this test goes red.
func TestChildDrainQuestion_ReachesParent(t *testing.T) {
	ctx := context.Background()
	f, parentID, childID, delivered := childDelegationFixture(t, attemptFixtureOpts{
		tools: []fantasy.AgentTool{tools.NewAskQuestionTool()}, handler: askQuestionResponse,
	})
	f.ledger.armDelegation(jobOf(f.ledger, parentID, "delegate-1"), jobResult{content: "first turn text"})
	select {
	case c := <-delivered:
		t.Fatalf("the delegation must stay open while the child owes a reaction: %+v", c)
	case <-time.After(200 * time.Millisecond):
	}

	require.ErrorAs(t, f.coord.wakeSession(ctx, childID, true), new(*AwaitingAnswerError))

	select {
	case c := <-delivered:
		require.Equal(t, "delegate-1", c.ToolCallID)
		require.False(t, c.IsError, "a question is not a failure")
		require.Contains(t, c.Content, "SUB-AGENT QUESTION")
		require.Contains(t, c.Content, "which one?")
		require.Contains(t, c.Content, `resume_session_id="`+childID+`"`)
	case <-time.After(10 * time.Second):
		t.Fatal("the delegation never released after the child asked its question")
	}
	require.True(t, f.coord.autoResumeSuspended(childID), "the pending question suspends the child's automatic turns")
}

// TestChildScope_DeferredDebtReleasesDelegation: debt the policy will never
// let the child act on (here: Stop's suspension) is not "open scope" -- the
// delegation releases instead of hanging its parent.
//
// Revert-check: the policy-blind scope read (ScopeOpen) in
// childScopeOpenAcrossProcesses keeps the delegation open and this test goes
// red on the timeout.
func TestChildScope_DeferredDebtReleasesDelegation(t *testing.T) {
	f, parentID, childID, delivered := childDelegationFixture(t, attemptFixtureOpts{noIdle: true})
	f.coord.suspendAutoResume(childID)

	f.ledger.armDelegation(jobOf(f.ledger, parentID, "delegate-1"), jobResult{content: "first turn text"})

	select {
	case c := <-delivered:
		require.Equal(t, "delegate-1", c.ToolCallID)
	case <-time.After(10 * time.Second):
		t.Fatal("deferred debt must not hold the delegation open")
	}
}
