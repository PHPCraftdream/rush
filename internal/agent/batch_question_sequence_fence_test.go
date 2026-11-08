// Sequence-fence regression (task 34, P1): the held-Ask delivery fence used
// to check only the DEPTH, so two questions of the SAME incarnation at the
// SAME depth (Q1 answered, then Q2 asked) were indistinguishable: a stale
// answer message captured before an interrupted delivery (R1) could resolve
// the WRONG question when injected late.
//
// TestStaleAnswerCannotResolveNextQuestionSameDepth pins the barrier at the
// execution-rig level (real agent-kind root, real held asks, real
// Runner.Inject seam).
//
// Mutation caught: deleting the sequence check in batchAgentScope.deliver
// lets R1's stale wire ({"depth":1,"sequence":1}) resolve Q2 — depth alone
// cannot distinguish two questions of one incarnation. Fences NOT weakened:
// stopped/closed still take precedence (deliver checks them first), depth is
// still checked before the sequence, and the token/generation fences are
// unchanged (stale incarnations still reject through the same code path).
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestStaleAnswerCannotResolveNextQuestionSameDepth(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	leaf := newGatedLeaf("view")
	// The parking agent turn asks TWICE at depth 1 and records both answers;
	// it finishes only after the second answer arrived.
	var mu sync.Mutex
	var answers []string
	record := func(answer json.RawMessage) {
		// The wire answer is the JSON-ENCODED string (the sink marshals
		// batchAgentInjectMessage{Text: mustJSONString(text)}), so decode
		// before comparing; a decode failure is recorded as a failure marker.
		var s string
		if err := json.Unmarshal(answer, &s); err != nil {
			s = fmt.Sprintf("<answer-decode-error: %v>", err)
		}
		mu.Lock()
		answers = append(answers, s)
		mu.Unlock()
		tl.addf("answer decoded: %s", s)
	}
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	factory := newBatchAgentTestFactory(turn)
	factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
		go func() {
			// Production binds the child session before any ask
			// (batch_install_question.go:35 refuses an empty ChildSessionID);
			// session_notices has no FK on the child id, so no child session
			// row is needed. Mirrors batch_answer_inject_test.go.
			host.BindChildSession(context.Background(), "sess-child-seq")
			ask := func(q string) {
				answer, err := host.Ask(context.Background(), 1, q)
				if err != nil {
					t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextErrorResponse(err.Error())}, nil)
				} else {
					record(answer)
				}
			}
			ask("first?")
			ask("second?")
			t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextResponse("done")}, nil)
			t2.finish()
			t2.exitNow()
		}()
	}
	reader := newBatchAgentTestReader(false, false)
	env := newCutoverEnvDeps(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64},
		nil, BatchAgentDeps{Runs: factory, Descendants: reader, Poll: time.Millisecond},
		[]fantasy.AgentTool{leaf})
	releaseAllAt(t, leaf)
	root := "root-stale-seq"
	tl.addf("agent root %s submitted", root)
	payload, err := json.Marshal(batchLeafPayload{
		Tool: "agent", ToolCallID: "call-stale-seq", SessionID: env.sessionID,
		Input: json.RawMessage(`{}`), RegistryID: env.registryID, QuestionRoute: true,
	})
	require.NoError(t, err)
	_, err = env.install.RunnerHandle.Submit(env.ctx, jobbatch.SubmitRequest{
		Root: jobbatch.ID(root),
		Spec: jobbatch.Spec{Task: &jobbatch.TaskSpec{Kind: batchKindAgent, Payload: payload}},
		Ref:  "call-stale-seq",
	})
	require.NoError(t, err, "the agent root is admitted")
	select {
	case <-factory.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the agent run factory never started")
	}
	runner := env.install.RunnerHandle.runner
	pendingNotice := func(t *testing.T, sequence string) string {
		t.Helper()
		var text string
		require.NoError(t, env.conn.QueryRowContext(env.ctx,
			`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending' AND text LIKE ?3`,
			env.sessionID, session.NoticeKindChildQuestion, `%sequence=`+sequence+`%`).Scan(&text))
		return text
	}

	// REQUIRED ORDER:
	// R1 capture (stale message built while Q1 held, BEFORE any delivery)
	//   → answer Q1 (accepted) → Q2 held+published → release R1 (the stale
	// inject lands only now) → rejection ErrBatchAgentNotHeld → Q2 still
	// held → answer-two accepted → answers exactly
	// ["answer-one","answer-two"].
	//
	// Q1 held and published (the counter starts 0 and increments before each
	// publish, so the FIRST question of this execution is sequence 1).
	require.Eventually(t, func() bool {
		var n int
		return env.conn.QueryRowContext(env.ctx,
			`SELECT COUNT(*) FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending' AND text LIKE '%sequence=1%'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&n) == nil && n == 1
	}, 5*time.Second, 5*time.Millisecond, "Q1 is held and published as sequence 1")
	tl.addf("Q1 published (sequence=1, held depth 1)")
	q1 := pendingNotice(t, "1")
	node, _, ok := parseBatchQuestionMarker(q1)
	require.True(t, ok, "the notice carries the production marker: %q", q1)
	require.Contains(t, q1, "depth=1")
	require.Contains(t, q1, "first?")

	// R1's captured wire: the STALE message is built ONCE, before any
	// delivery, and only injected later (after Q2 is asked).
	stale, merr := json.Marshal(batchAgentInjectMessage{Depth: 1, Sequence: 1, Text: json.RawMessage(`"stale-one"`)})
	require.NoError(t, merr)
	tl.addf("R1 stale wire captured before any delivery (depth=1 sequence=1)")

	// The proper answer resolves Q1.
	require.NoError(t, runner.Inject(env.ctx, jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"sequence":1,"text":"answer-one"}`)),
		"Q1's answer is accepted at its own sequence")
	tl.addf("answer-one accepted at sequence 1")
	// RESOLUTION LIFECYCLE: acceptance resolves the incarnation's published
	// questions (delivery='void', wake=0) — the notice must never outlive
	// the hold it advertised.
	require.Eventually(t, func() bool {
		var delivery string
		var wake int64
		return env.conn.QueryRowContext(env.ctx,
			`SELECT delivery, wake FROM session_notices WHERE owner = ?1 AND kind = ?2 AND text LIKE '%sequence=1%'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&delivery, &wake) == nil &&
			delivery == "void" && wake == 0
	}, 5*time.Second, 5*time.Millisecond, "the sequence=1 notice is resolved (void, wake=0) after answer-one")
	tl.addf("sequence=1 notice resolved: delivery=void wake=0")
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(answers) >= 1
	}, 5*time.Second, 5*time.Millisecond, "Q1's asker received its answer")

	// The turn asks again at the SAME depth: Q2 is held and published as
	// sequence 2.
	require.Eventually(t, func() bool {
		var n int
		return env.conn.QueryRowContext(env.ctx,
			`SELECT COUNT(*) FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending' AND text LIKE '%sequence=2%'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&n) == nil && n == 1
	}, 5*time.Second, 5*time.Millisecond, "Q2 is held and published as sequence 2")
	tl.addf("Q2 published (sequence=2, held depth 1)")
	q2 := pendingNotice(t, "2")
	require.Contains(t, q2, "depth=1")
	require.Contains(t, q2, "second?")

	// THE BARRIER: R1's stale wire (sequence 1) must NOT resolve Q2, even at
	// the same depth of the same incarnation.
	require.ErrorIs(t, runner.Inject(env.ctx, jobbatch.ID(node), stale),
		ErrBatchAgentNotHeld, "the stale sequence-1 wire cannot resolve the sequence-2 hold")
	tl.addf("R1 stale inject rejected ErrBatchAgentNotHeld; Q2 still held")

	// Q2 is STILL held: the correct sequence-2 answer is accepted (had the
	// stale injection resolved Q2, this follow-up would fail NotHeld).
	require.NoError(t, runner.Inject(env.ctx, jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"sequence":2,"text":"answer-two"}`)),
		"Q2's answer is accepted at its own sequence")
	tl.addf("answer-two accepted at sequence 2")
	require.Eventually(t, func() bool {
		var delivery string
		var wake int64
		return env.conn.QueryRowContext(env.ctx,
			`SELECT delivery, wake FROM session_notices WHERE owner = ?1 AND kind = ?2 AND text LIKE '%sequence=2%'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&delivery, &wake) == nil &&
			delivery == "void" && wake == 0
	}, 5*time.Second, 5*time.Millisecond, "the sequence=2 notice is resolved (void, wake=0) after answer-two")
	tl.addf("sequence=2 notice resolved: delivery=void wake=0")
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(answers) >= 2
	}, 5*time.Second, 5*time.Millisecond, "Q2's asker received its answer")

	// Deterministic negatives on the now-empty hold.
	require.ErrorIs(t, runner.Inject(env.ctx, jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"sequence":2,"text":"double"}`)), ErrBatchAgentNotHeld,
		"a second answer at the same sequence cannot double-answer")
	require.ErrorIs(t, runner.Inject(env.ctx, jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"sequence":3,"text":"future"}`)), ErrBatchAgentNotHeld,
		"a future sequence cannot answer a never-published hold")
	require.ErrorIs(t, runner.Inject(env.ctx, jobbatch.ID(node),
		json.RawMessage(`{"depth":1,"text":"zeroseq"}`)), ErrBatchAgentNotHeld,
		"a zero-sequence wire can only match a never-published hold")

	// The child's recorded answers are exactly the two proper answers —
	// never the stale text.
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"answer-one", "answer-two"}, answers,
		"the stale wire never reached any asker")
	tl.addf("answers decoded == [answer-one answer-two]")

	// Settle hygiene: release the answers' incarnation through the real
	// engine and clean the fixture owners.
	reader.set(false, true)
	require.Eventually(t, func() bool {
		return env.rootTerminal(root) == "completed"
	}, 5*time.Second, 5*time.Millisecond, "the answered incarnation settles completed")
	require.NoError(t, env.install.Runner.Release(env.ctx, jobbatch.ID(root)))
}
