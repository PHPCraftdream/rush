// A delegated child's Drain turn (R2B-1 B, child half): a question the child
// asks while reacting to its own notice reaches the parent through the
// delegation's release, and debt the policy will never let the child act on
// (deferred: a question, Stop) does not hold the delegation open.
package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
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
// turn, AFTER writing a sentence; the question is the reaction (debt closed),
// automatic turns suspend, the delegation releases, and the parent gets the
// question -- as a paused sub-agent it can resume, with the sentence kept as
// its preamble, not as a failure and not as "no final text".
//
// Revert-check: letting the text branch win in refreshSubAgentCompletion
// (question check after it) delivers a FAILED delegation without the question
// block and this test goes red; dropping subAgentQuestionFromFinish delivers
// the stale text as a failure likewise.
func TestChildDrainQuestion_ReachesParent(t *testing.T) {
	ctx := context.Background()
	f, parentID, childID, delivered := childDelegationFixture(t, attemptFixtureOpts{
		tools: []fantasy.AgentTool{tools.NewAskQuestionTool()}, handler: textThenAskQuestionResponse,
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
		require.Contains(t, c.Content, "Checked the job output; one detail is unclear.", "the child's own words stay as the question's preamble")
		require.Less(t, strings.Index(c.Content, "Checked the job output"), strings.Index(c.Content, "SUB-AGENT QUESTION"))
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

// TestRunSubAgent_FirstTurnQuestionKeepsPreamble: the child's FIRST turn ends
// on a question it asked after writing a sentence: the parent's tool response
// is the question (not an error) and keeps the sentence as its preamble.
//
// Revert-check: building the response from subAgentQuestionText alone (no
// childQuestionPreamble) drops the sentence and this test goes red.
func TestRunSubAgent_FirstTurnQuestionKeepsPreamble(t *testing.T) {
	env := testEnv(t)
	coord := newWorkerToolTestCoordinator(t, env, false)
	c6ConfigureProvider(t, coord, "smart-provider", "pinned-model", "key", nil)
	parent, err := env.sessions.Create(t.Context(), "question parent")
	require.NoError(t, err)

	agent := &mockSessionAgent{model: c6BuildModel(t, coord, true)}
	agent.runFunc = func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		qe := &AwaitingAnswerError{Question: "which one?", SessionID: call.SessionID}
		title, details := awaitingAnswerStoppedFinishText(qe)
		_, createErr := env.messages.Create(ctx, call.SessionID, message.CreateMessageParams{
			Role: message.Assistant,
			Parts: []message.ContentPart{
				message.TextContent{Text: "Checked the job output; one detail is unclear."},
				message.Finish{Reason: message.FinishReasonError, Message: title, Details: details},
			},
		})
		require.NoError(t, createErr)
		return nil, qe
	}
	coord.currentAgent = agent

	resp, err := coord.runSubAgent(t.Context(), subAgentParams{
		Agent: agent, SessionID: parent.ID, AgentMessageID: "message", ToolCallID: "tool",
		Prompt: "do the work", SessionTitle: "question child",
	})
	require.NoError(t, err)
	require.False(t, resp.IsError, "a question is not a failure")
	require.Contains(t, resp.Content, "Checked the job output; one detail is unclear.")
	require.Contains(t, resp.Content, "SUB-AGENT QUESTION")
	require.Contains(t, resp.Content, "which one?")
	require.Less(t, strings.Index(resp.Content, "Checked the job output"), strings.Index(resp.Content, "SUB-AGENT QUESTION"))
}

// TestChildScope_RerunHoldKeepsDelegationOpen (R3C-5): a rerun holds the
// child's automatic turns while it cancels, truncates and hands off. The hold
// is temporary, so the child's scope reads as paced (open), not deferred: the
// parent's delegation is not released with stale text during the hold, and the
// debt is owed again once the hold is released.
//
// Revert-check: answering "deferred" for a held session (the old verdict)
// reads the child as drained and delivers the delegation during the hold: this
// test goes red on the early delivery.
func TestChildScope_RerunHoldKeepsDelegationOpen(t *testing.T) {
	ctx := context.Background()
	f, parentID, childID, delivered := childDelegationFixture(t, attemptFixtureOpts{noIdle: true})
	release := f.coord.HoldAutomaticTurns(childID)
	t.Cleanup(release)

	st, err := f.coord.CLIScope(ctx, childID)
	require.NoError(t, err)
	require.Equal(t, DrainPaced, st.Drain, "a hold is a temporary pause, not a refusal")
	require.True(t, st.RetryAt.IsZero(), "no clock: the tick and the release retry it")

	f.ledger.armDelegation(jobOf(f.ledger, parentID, "delegate-1"), jobResult{content: "first turn text"})
	select {
	case c := <-delivered:
		t.Fatalf("the delegation must stay open while a rerun holds the child: %+v", c)
	case <-time.After(300 * time.Millisecond):
	}

	release()
	st, err = f.coord.CLIScope(ctx, childID)
	require.NoError(t, err)
	require.Equal(t, DrainOwed, st.Drain, "the debt is owed again after the hold")
}
