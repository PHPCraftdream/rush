// Held-вопрос отклоняет resume до bind; ответ через inject освобождает исходный Run.
// Bind-spy превращает удаление guard в assertion failure, а не panic stub-agent.
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestResumeOfHeldQuestionChildRefusedBeforeBind(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	ctx := context.Background()
	env := newQuestionInstall(t)
	root := env.submitAgentRoot()
	tl.addf("agent root submitted")

	// 1. Wait for the pending child_question notice; parse it; re-parent the
	// child exactly like TestBatchInjectBridge_HappyPathReleasesHeldQuestion.
	var text string
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(ctx,
			`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&text) == nil
	}, 5*time.Second, 5*time.Millisecond, "the held question is published")
	node, token, ok := parseBatchQuestionMarker(text)
	require.True(t, ok, "the notice carries the production marker: %q", text)
	tl.addf("question published: node=%s token=%d", node, token)
	var childID string
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT id FROM sessions WHERE title = 'q8 child' LIMIT 1`).Scan(&childID))
	require.Equal(t, batchQuestionMarkerChild(text), childID)
	_, err := env.conn.ExecContext(ctx,
		`UPDATE sessions SET parent_session_id = ?1 WHERE id = ?2`, env.sessionID, childID)
	require.NoError(t, err)

	// 2. Coordinator with the live held-question route reader bound.
	coord, _ := newControlCoordinator(t)
	coord.sessions = env.sessions
	coord.messages = env.messages
	bindBatchNodeFacts(t, coord, env.conn, t.TempDir())

	// 3. FULL exact id: the guard refuses BEFORE any state mutation, with an
	// actionable message naming the exact inject_agent route. The resume
	// refusal path touches NOTHING from params.Agent (the refusal returns
	// before any model/driver use), so the zero-value stub only needs to
	// satisfy the SessionAgent interface.
	bindCalls := 0
	resumeCtx := context.WithValue(ctx, batchAgentLifecycleContextKey{}, batchAgentLifecycle{
		BindChildSession: func(context.Context, string) error {
			bindCalls++
			return errors.New("unexpected bind behind held-question guard")
		},
	})
	resp, err := coord.runSubAgent(resumeCtx, subAgentParams{
		SessionID: env.sessionID, Agent: &controlStubAgent{},
		ResumeSessionID: childID,
	})
	require.NoError(t, err, "a held-question refusal is a tool error, not a Go error")
	require.True(t, resp.IsError, "the resume is refused with a tool error")
	tl.addf("full-id resume refused with the inject_agent hint")
	require.Contains(t, resp.Content, "inject_agent", "the refusal names the answer tool")
	require.Contains(t, resp.Content, childID, "the refusal names the exact child id")
	require.Contains(t, strings.ToLower(resp.Content), "held")
	require.Contains(t, resp.Content, node, "the refusal names the held batch node")

	// 4. Nothing happened behind the refusal: no driver registered, no new
	// run queued, the node is still live and the question still actionable.
	_, registered := coord.subAgentDrivers.get(childID)
	require.False(t, registered, "no driver was registered behind the refusal")
	require.Equal(t, 1, env.leaf.runCount(), "no new generation was launched")
	var state string
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
		node, env.sessionID).Scan(&state))
	require.NotEqual(t, "completed", state, "the held node is still live")
	src, srcOK := coordinatorBatchCutoverFacts(coord).(BatchHeldQuestionSource)
	require.True(t, srcOK)
	routes, rerr := src.ActionableHeldQuestions(ctx, env.sessionID)
	require.NoError(t, rerr)
	require.NotEmpty(t, routes, "the held question is STILL actionable")
	require.Equal(t, token, routes[0].Token, "the same incarnation is still held")
	tl.addf("routes still actionable, same token")

	// The mx-app incident shape: without the guard the resume would bind a
	// NEW delegation to the SAME child id and complete it with a false
	// empty outcome while the original hold stayed parked. Pin the
	// structural absence: exactly ONE root for the owner and exactly ONE
	// batch node naming the child (the original incarnation's).
	var rootCount int
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT count(*) FROM batch_nodes WHERE owner_session_id = ?1 AND kind = 'batch' AND parent_id IS NULL`,
		env.sessionID).Scan(&rootCount))
	require.Equal(t, 1, rootCount, "the refused resume created NO new delegation root")
	var boundNodes int
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT count(*) FROM batch_nodes WHERE owner_session_id = ?1 AND child_session_id = ?2`,
		env.sessionID, childID).Scan(&boundNodes))
	require.Equal(t, 1, boundNodes, "no second batch node was bound to the SAME child id")
	tl.addf("one root, one child-bound node: no new delegation behind the refusal")

	// The guard does not degrade after the first refusal: the SAME full-id
	// resume is refused again, with the same actionable route.
	resp2, err2 := coord.runSubAgent(resumeCtx, subAgentParams{
		SessionID: env.sessionID, Agent: &controlStubAgent{},
		ResumeSessionID: childID,
	})
	require.NoError(t, err2, "the second refusal is still a tool error, not a Go error")
	require.True(t, resp2.IsError, "the SECOND resume with the same full id is refused again")
	require.Zero(t, bindCalls, "held-question refusal must precede child binding")
	require.Contains(t, resp2.Content, "inject_agent", "the second refusal still names the answer tool")
	tl.addf("second full-id resume refused identically (guard idempotent)")

	// 5. Foreign-child safety: a resume of an UNINVOLVED session is not
	// refused by someone else's question.
	require.Equal(t, "", coord.batchHeldQuestionResumeRefusal(ctx, env.sessionID, "sess-not-involved"),
		"an uninvolved session id is never refused by another child's question")

	// 6. Capability-absent legacy: a coordinator without bound facts never
	// refuses — legacy resumes keep working.
	legacy, _ := newControlCoordinator(t)
	require.Equal(t, "", legacy.batchHeldQuestionResumeRefusal(ctx, "any-owner", "any-child"),
		"without the held-question capability the guard is inert")

	// 7. Correct answer through the sink releases the hold; afterwards the
	// guard no longer refuses a resume of the same child (legacy resume
	// behaviour after the question is gone is preserved).
	sink, serr := NewBatchHeldAnswerSink(env.conn, env.install.Runner)
	require.NoError(t, serr)
	SetCoordinatorBatchHeldAnswerSink(coord, sink)
	t.Cleanup(func() { SetCoordinatorBatchHeldAnswerSink(coord, nil) })
	out, ierr := coord.InjectAgent(ctx, env.sessionID, childID, "yes, proceed", false)
	require.NoError(t, ierr, "the bridge delivers the answer")
	require.Contains(t, out, "SAME batch execution")
	tl.addf("sink inject accepted; hold resolved")
	var doneToken int64
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(ctx,
			`SELECT state, token FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			node, env.sessionID).Scan(&state, &doneToken) == nil && state == "completed"
	}, 5*time.Second, 5*time.Millisecond, "the answered incarnation settles completed")
	require.Equal(t, token, doneToken, "the incarnation token is unchanged")
	require.Equal(t, 1, env.leaf.runCount(), "the answer ran the EXISTING incarnation, not a new one")
	require.Equal(t, "", coord.batchHeldQuestionResumeRefusal(ctx, env.sessionID, childID),
		"the answered question no longer refuses a resume")

	// Still exactly one root and one child-bound node: the answer released
	// the ORIGINAL incarnation — no replacement delegation appeared at any
	// point (the mx-app false-success shape is structurally absent).
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT count(*) FROM batch_nodes WHERE owner_session_id = ?1 AND kind = 'batch' AND parent_id IS NULL`,
		env.sessionID).Scan(&rootCount))
	require.Equal(t, 1, rootCount, "still exactly ONE delegation root after the release")
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT count(*) FROM batch_nodes WHERE owner_session_id = ?1 AND child_session_id = ?2`,
		env.sessionID, childID).Scan(&boundNodes))
	require.Equal(t, 1, boundNodes, "still exactly ONE batch node bound to the child")
	tl.addf("post-release: root and bound-node counts unchanged")

	// The real-answer oracle: the ORIGINAL hold received the answer text
	// (not a false completed outcome of a new delegation).
	_, _, _, _, _, rawAnswer := env.leaf.snapshot()
	require.Equal(t, "yes, proceed", string(rawAnswer),
		"the ORIGINAL hold was answered through the correct inject, not a false completed outcome of a new delegation")
	tl.addf("leaf snapshot carries the real answer text")

	// Release hygiene mirrors TestBatchInjectBridge_HappyPathReleasesHeldQuestion.
	require.NoError(t, env.install.Runner.Release(ctx, root))
	env.releaseConstruction()
	require.Eventually(t, func() bool { return env.install.DiskBindings.ownerCount() == 0 },
		5*time.Second, time.Millisecond, "no provider owner remains")
}
