package agent

// AW-a5 accepted-answer proof (R12-E round 5): a REAL agent-kind batch child
// parks in host.Ask, the production publishQuestion writes the durable
// child_question notice under the execution's OWN node/token, and the
// answer is ACCEPTED through the REAL jobbatch Runner.Inject seam
// (runner_requests.go: Inject -> handle.execution.(Messenger).Inject ->
// batchAgentExecution.Inject -> scope.deliver at the held depth).

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAWa5AgentChildAnswerViaRunnerInject(t *testing.T) {
	leaf := newGatedLeaf("view")
	// The parking agent turn: ask at depth 1; the turn only finishes once
	// the injected answer arrives (the askAndShape production-shaped host
	// ask from the R11G rig).
	const answer = `42 — proceed`
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	factory := newBatchAgentTestFactory(turn)
	factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
		// Production binds the child session inside the agent run
		// (batch_exec_agent.go launches the run with
		// BindChildSession: e.bindChildSession); the fixture mirrors that
		// before asking. session_notices has no FK on a child id (its only
		// reference is owner -> sessions), so no child session row is
		// needed -- the id only lands in the marker text and the publisher
		// struct's ChildSessionID (which batch_install_question requires
		// nonempty).
		host.BindChildSession(context.Background(), "sess-child-a5i")
		askAndShape(t2, host, 1, "continue?")
	}
	reader := newBatchAgentTestReader(false, false)
	env := newCutoverEnvDeps(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64},
		nil, BatchAgentDeps{Runs: factory, Descendants: reader, Poll: time.Millisecond},
		[]fantasy.AgentTool{leaf})
	releaseAllAt(t, leaf)
	coord := newCutoverCoordinator(t, env)
	cutoverBindFacts(t, coord, env)

	// Submit a ONE-ITEM AGENT root through the REAL runner handle: the
	// default executor's agent family (wired with the parking factory)
	// starts the incarnation; askBatchQuestion publishes through the
	// PRODUCTION publisher with the execution's own node id/token.
	root := "root-a5i"
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: "call-a5i-agent", SessionID: env.sessionID,
		Input: json.RawMessage(`{}`), RegistryID: env.registryID,
	})
	require.NoError(t, err)
	_, err = env.install.RunnerHandle.Submit(env.ctx, jobbatch.SubmitRequest{
		Root: jobbatch.ID(root),
		Spec: jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload}},
		Ref:  "call-a5i-agent",
	})
	require.NoError(t, err, "the agent root is admitted")
	select {
	case <-factory.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the agent run factory never started")
	}

	// FIRST ASSERTION (static trace in the report): the production
	// publishQuestion (batch_exec_agent.go askBatchQuestion -> publishQuestion
	// -> batchCoordinatorQuestionPublisher.PublishChildQuestion ->
	// store.BatchPublishQuestion) wrote a pending child_question notice whose
	// text carries the marker header with THIS incarnation's node id and
	// nonzero token; BatchPublishQuestion verified the row is live and the
	// token matches before inserting.
	var text string
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(env.ctx,
			`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&text) == nil
	}, 5*time.Second, 5*time.Millisecond, "the production publisher wrote the held question")
	node, tok, ok := parseBatchQuestionMarker(text)
	require.True(t, ok, "the notice text carries the production marker: %q", text)
	require.NotEmpty(t, node)
	require.Greater(t, tok, int64(0), "the incarnation token is nonzero")
	require.Contains(t, text, "depth=1", "the marker stamps the held depth")
	require.Contains(t, text, "continue?", "the question body follows the marker")

	// The answer goes through the REAL Runner.Inject seam: ACCEPTED.
	runner := env.install.RunnerHandle.runner
	require.NoError(t, runner.Inject(env.ctx, jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"text":"`+answer+`"}`)),
		"the answer is accepted into the SAME incarnation")

	// The parked ask returned: the turn finished with the answer text.
	require.Eventually(t, func() bool {
		select {
		case <-turn.Done():
			return true
		default:
			return false
		}
	}, 5*time.Second, 5*time.Millisecond, "the parked ask woke on the injected answer")
	res := turn.Result()
	require.False(t, res.Response.IsError)
	require.True(t, strings.Contains(res.Response.Content, answer),
		"the answer text reached the asker, got %q", res.Response.Content)

	// The incarnation settles exactly once with the answer as its summary;
	// descendants confirmed terminal first (the rig reader).
	reader.set(false, true)
	require.Eventually(t, func() bool {
		return env.rootTerminal(root) == "completed"
	}, 5*time.Second, 5*time.Millisecond, "the answered node settles completed")
	require.False(t, cutoverStateLive(env.nodeState(root)), "the node is terminal after settling")

	// What production did to the question notice on acceptance (VERIFIED
	// shape, not fabricated): BatchPublishQuestion inserted the notice
	// pending+wake=1 and NOTHING in the acceptance path reacts or deletes
	// it -- scope.deliver only answers the in-memory hold. The notice is
	// consumed by the OWNER's ordinary notice pull (the node is terminal
	// now, so the pull no longer gates on it). Assert the durable shape:
	// still present, delivery pending, unreacted, wake=1.
	var delivery string
	var wake, reacted int64
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT delivery, wake, reacted FROM session_notices WHERE owner = ?1 AND kind = ?2`,
		env.sessionID, session.NoticeKindChildQuestion).Scan(&delivery, &wake, &reacted))
	require.Equal(t, "pending", delivery, "the question notice stays until the owner's pull")
	require.EqualValues(t, 1, wake, "the held question wakes the owner session")
	require.EqualValues(t, 0, reacted, "acceptance does not react the question notice")

	// A late answer on the terminal incarnation is refused: the public
	// guard in runner_requests.go returns ErrTerminal for a terminal node
	// BEFORE the executor (and its stale-generation check) is consulted.
	require.ErrorIs(t, runner.Inject(env.ctx, jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"text":"late"}`)), jobbatch.ErrTerminal,
		"a terminal incarnation rejects further answers")
	require.Zero(t, env.asyncJobsCount())
	env.logDebtRows(t)
	env.logNodeRows(t, root)
}
