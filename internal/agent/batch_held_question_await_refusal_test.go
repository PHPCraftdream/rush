// Reaction-independent held-question routing tests (production hang fix).
// The incident: the owner's reaction flow marked the child_question notice
// reacted=1 and its pull set delivery='done' while the child stayed held;
// the reacted=0-only readers then made the answer unroutable, the armed
// sleepAll held, and the child waited until the 45-minute watchdog killed
// the tool. These tests pin the invariant: a mere reaction must NOT make an
// undelivered answer invisible; the question stays actionable until an
// answer is ACCEPTED; after acceptance the notice closes so it never blocks
// await again.
package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestActionableHeldQuestionRoutesIgnoreReactionKeepFences: the store-level
// route read ignores `reacted` (and delivery='done') but keeps the
// incarnation fence and the child filter.
// Mutation caught: any `reacted = 0` (or delivery='pending'-only) predicate
// in the new reader makes the done/reacted=1 step below drop to zero
// routes; dropping the token/state/child fence makes a negative step below
// report a route.
func TestActionableHeldQuestionRoutesIgnoreReactionKeepFences(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	_, conn, ctx := newTestBatchStore(t)
	const owner = "sess-held-route-owner"
	const child = "sess-held-route-child"
	_, node1 := seedDelegation(t, conn, owner, "call-held-route-1", child, 7)
	commitMarkerNotice(t, conn, owner, node1, child, 7)
	tl.addf("seeded live question node=%s token=7", node1)

	routes, err := batchActionableHeldQuestionRoutes(ctx, conn, owner)
	require.NoError(t, err)
	require.Len(t, routes, 1, "the committed live question is one route")
	tl.addf("routes=1 (live fenced question)")
	require.Equal(t, node1, routes[0].NodeID)
	require.Equal(t, int64(7), routes[0].Token)
	require.Equal(t, 1, routes[0].Depth)
	require.Equal(t, int64(1), routes[0].Sequence)
	require.Equal(t, child, routes[0].ChildSessionID)
	require.Contains(t, routes[0].Question, "which one?")

	// The incident's pull+reaction: done + reacted must NOT hide the route.
	_, err = conn.ExecContext(ctx,
		`UPDATE session_notices SET delivery = 'done', reacted = 1 WHERE owner = ?1 AND kind = ?2`,
		owner, session.NoticeKindChildQuestion)
	require.NoError(t, err)
	routes, err = batchActionableHeldQuestionRoutes(ctx, conn, owner)
	require.NoError(t, err)
	require.Len(t, routes, 1, "a reacted, delivered question STAYS routable")
	tl.addf("after done+reacted: routes=1")

	// Fence negative: stale generation (the node row moved to a new token).
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET token = 8 WHERE node_id = ?1`, node1)
	require.NoError(t, err)
	routes, err = batchActionableHeldQuestionRoutes(ctx, conn, owner)
	require.NoError(t, err)
	require.Empty(t, routes, "a stale-token notice is not a route")
	tl.addf("after token bump: routes=0")

	// Fence negative: terminal incarnation.
	_, node2 := seedDelegation(t, conn, owner, "call-held-route-term", child, 9)
	commitMarkerNotice(t, conn, owner, node2, child, 9)
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET state = 'completed' WHERE node_id = ?1`, node2)
	require.NoError(t, err)
	routes, err = batchActionableHeldQuestionRoutes(ctx, conn, owner)
	require.NoError(t, err)
	require.Empty(t, routes, "a terminal incarnation is not a route")

	// Удалённый node — доказанное отсутствие incarnation, не отказ SQL.
	_, ghostNode := seedDelegation(t, conn, owner, "call-held-route-ghost", child, 11)
	commitMarkerNotice(t, conn, owner, ghostNode, child, 11)
	_, err = conn.ExecContext(ctx, `DELETE FROM batch_nodes WHERE node_id = ?1`, ghostNode)
	require.NoError(t, err)
	routes, err = batchActionableHeldQuestionRoutes(ctx, conn, owner)
	require.NoError(t, err, "a deleted node cannot own a current held question")
	require.Empty(t, routes)
	tl.addf("ghost notice retained; missing incarnation contributes no route")

	// Fence negative: retired ROOT (delivery='void'). Mutation caught:
	// dropping the root-void join term reports the still-live child's
	// question actionable.
	_, retiredNode := seedDelegation(t, conn, owner, "call-held-route-retired", child, 13)
	commitMarkerNotice(t, conn, owner, retiredNode, child, 13)
	var retiredRoot string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT root_id FROM batch_nodes WHERE node_id = ?1`, retiredNode).Scan(&retiredRoot))
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET delivery='void', wake=0 WHERE node_id = ?1`, retiredRoot)
	require.NoError(t, err)
	routes, err = batchActionableHeldQuestionRoutes(ctx, conn, owner)
	require.NoError(t, err)
	require.Empty(t, routes, "a question of a retired root is not a route")
	tl.addf("retired root: routes=0 (root-void join term)")

	// Fence honesty: an already-cancelled context makes every fence read
	// fail; the whole answer must be conservative (error non-nil), not an
	// empty proven-absence.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	_, fenceErr := batchActionableHeldQuestionRoutes(cctx, conn, owner)
	require.Error(t, fenceErr, "an unreadable fence read returns the error, never proven absence")
	tl.addf("cancelled-ctx fence read: error non-nil")

	// Child filter: a second question naming a DIFFERENT child never
	// attributes to a third child id.
	const childB = "sess-held-route-child-b"
	_, nodeB := seedDelegation(t, conn, owner, "call-held-route-b", childB, 12)
	commitMarkerNotice(t, conn, owner, nodeB, childB, 12)
	routes, err = batchActionableHeldQuestionRoutes(ctx, conn, owner)
	require.NoError(t, err)
	require.Len(t, routes, 1, "only the live fenced question routes")
	require.Equal(t, childB, routes[0].ChildSessionID)
	for _, r := range routes {
		require.NotEqual(t, "sess-held-route-child-other", r.ChildSessionID,
			"the child filter never attributes another child's question")
	}

	// delivery='void' is excluded.
	_, err = conn.ExecContext(ctx,
		`UPDATE session_notices SET delivery = 'void', wake = 0 WHERE owner = ?1 AND kind = ?2`,
		owner, session.NoticeKindChildQuestion)
	require.NoError(t, err)
	routes, err = batchActionableHeldQuestionRoutes(ctx, conn, owner)
	require.NoError(t, err)
	require.Empty(t, routes, "a voided notice is not a route")
	tl.addf("voided notice: routes=0")
}

// TestAwaitTasksRefusesHeldQuestionWithoutSleepOrTimer: the top-level
// await refuses (both modes) when the ONLY blocker is a held child
// question in the incident state (delivery='done', reacted=1), without
// arming the arbiter sleep; answering through the REAL sink unblocks.
// Mutation caught: the old code armed sleepAll and scheduled a wake; the
// reacted=0-only bridge missed the answer.
func TestAwaitTasksRefusesHeldQuestionWithoutSleepOrTimer(t *testing.T) {
	for _, mode := range []string{"all", "any"} {
		t.Run(mode, func(t *testing.T) {
			leaf := newGatedLeaf("view")
			env := newCutoverEnv(t, jobbatch.Limits{MaxConcurrent: 8, MaxDepth: 8, MaxNodes: 64}, leaf)
			releaseAllAt(t, leaf)
			tl := newQuestionTestTimeline(t)
			tl.addf("mode=%s env built", mode)
			coord := newCutoverCoordinator(t, env)
			cutoverBindFacts(t, coord, env)
			callID := "held-await-" + mode
			env.invokeDetached(callID, "view", leaf)
			leaf.waitStarted(t, callID)
			// A SECOND detached root keeps live work in the scope besides the
			// questioned root: the held-question refusal must STILL fire.
			env.invokeDetached(callID+"-second", "view", leaf)
			leaf.waitStarted(t, callID+"-second")
			tl.addf("two detached roots live: %s, %s", callID, callID+"-second")
			root := env.rootID(callID)
			var nodeID string
			var token int64
			require.NoError(t, env.conn.QueryRowContext(env.ctx,
				`SELECT node_id, token FROM batch_nodes WHERE root_id = ?1 AND node_id != root_id AND state IN ('pending','starting','running','cancelling') ORDER BY ordinal LIMIT 1`,
				root).Scan(&nodeID, &token))
			const child = "held-await-child"
			require.NoError(t, (batchCoordinatorQuestionPublisher{store: env.store}).PublishChildQuestion(env.ctx, batchChildQuestionNotice{
				Owner: env.sessionID, ChildSessionID: child, NodeID: nodeID,
				Token: uint64(token), Depth: 1, Sequence: 1, Question: "which option?",
			}))
			// The incident state: pulled (done) and reacted.
			_, err := env.conn.ExecContext(env.ctx,
				`UPDATE session_notices SET delivery = 'done', reacted = 1 WHERE owner = ?1 AND kind = ?2`,
				env.sessionID, session.NoticeKindChildQuestion)
			require.NoError(t, err)
			tl.addf("incident state written: delivery=done reacted=1 node=%s", nodeID)

			// TOOL-LEVEL segment: with max_wait>0 the await_tasks TOOL must
			// still refuse BEFORE any timer/sleep/StopTurn. Old code: no
			// error at all and StopTurn true — both assertions distinguish.
			ingress, err := NewBatchWorkIngress(env.install.Runtime, env.registries)
			require.NoError(t, err)
			require.NoError(t, BindCoordinatorBatchControls(coord, ingress, nil, env.live))
			tool := tools.NewBatchAwaitTasksTool(coord)
			toolCtx := context.WithValue(env.ctx, tools.SessionIDContextKey, env.sessionID)
			tresp, terr := tool.Run(toolCtx, fantasy.ToolCall{
				ID: "held-await-tool-" + mode, Name: tools.AwaitTasksToolName,
				Input: `{"until":"all","max_wait_seconds":60}`,
			})
			require.NoError(t, terr)
			require.True(t, tresp.IsError, "the tool surfaces the held question as an error response")
			require.False(t, tresp.StopTurn, "the refusal fires before any sleep/StopTurn")
			require.Contains(t, tresp.Content, "held child batch question")
			tl.addf("tool-level await refused (max_wait=60): isError=true stopTurn=false")

			_, err = coord.AwaitTasks(env.ctx, env.sessionID, mode, 0)
			require.ErrorIs(t, err, ErrBatchAwaitHeldQuestion, "the held question refuses the await even with a SECOND live root")
			require.Contains(t, err.Error(), nodeID)
			require.Contains(t, err.Error(), child)
			require.False(t, coord.arb.snapshot(env.sessionID).SleepAll,
				"the refusal fires BEFORE any sleep is armed")
			tl.addf("top-level await refused with two live roots")

			// Answer through the REAL sink: the reacted notice still routes.
			// The fake runner is deliberate: the env's delegation node is a
			// GENERIC task execution, which does not implement Messenger, so
			// a real Runner.Inject here can only panic-fail inside
			// runner_requests.go; a recording fake satisfies the sink while
			// the store-level assertions (routing, close-on-acceptance) stay
			// real. Real-runner acceptance through a genuinely held AGENT
			// child is TestHeldQuestionIncidentChain.
			sink, serr := NewBatchHeldAnswerSink(env.conn, &bridgeFakeRunner{})
			require.NoError(t, serr)
			_, aerr := sink.DeliverHeldAnswer(env.ctx, env.sessionID, child, "go ahead")
			require.NoError(t, aerr, "the reacted question routes through the real sink")
			var delivery string
			var wake int64
			require.NoError(t, env.conn.QueryRowContext(env.ctx,
				`SELECT delivery, wake FROM session_notices WHERE owner = ?1 AND kind = ?2`,
				env.sessionID, session.NoticeKindChildQuestion).Scan(&delivery, &wake))
			require.Equal(t, "void", delivery, "close-on-acceptance")
			require.Equal(t, int64(0), wake)
			src, ok := coordinatorBatchCutoverFacts(coord).(BatchHeldQuestionSource)
			require.True(t, ok)
			routes, rerr := src.ActionableHeldQuestions(env.ctx, env.sessionID)
			require.NoError(t, rerr)
			require.Empty(t, routes, "the answered question is no longer a route")
			tl.addf("sink answer accepted; notice void; routes empty")

			// The second await no longer refuses with the held-question
			// error; the live root is still running, so success is expected.
			_, err = coord.AwaitTasks(env.ctx, env.sessionID, mode, 0)
			require.NotErrorIs(t, err, ErrBatchAwaitHeldQuestion)
			require.NoError(t, err)
			tl.addf("follow-up await succeeded with both roots live")
		})
	}
}

// TestBatchAwaitWorkerBreaksWhenQuestionAppearsMidWait: a question that
// appears AFTER the initial evaluation breaks the in-tool wait with the
// held-question refusal instead of running to the cap.
// Mutation caught: without the HeldQuestions dep wired into the evaluation
// loop, the wait runs to the cap/timeout and no refusal error surfaces.
func TestBatchAwaitWorkerBreaksWhenQuestionAppearsMidWait(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	root := batchAwaitRootRow("held-mid-root", "running")
	h := newBatchAwaitHarness(t, root)
	h.setLevel("held-mid-root", root, batchAwaitChildRow("held-mid-root", "held-mid-root.1", "running"))
	calls := &atomic.Int64{}
	params := h.params("all", 0)
	params.PollInterval = time.Millisecond
	params.WaitCap = 50 * time.Millisecond
	params.HeldQuestions = func(context.Context) ([]BatchHeldQuestionRoute, error) {
		if calls.Add(1) == 1 {
			return nil, nil // no question at the initial evaluation
		}
		return []BatchHeldQuestionRoute{{NodeID: "held-mid-node", ChildSessionID: "held-mid-child", Token: 3, Depth: 1, Sequence: 1}}, nil
	}
	done := make(chan error, 1)
	go func() {
		_, err := BatchAwaitWorker(context.Background(), params)
		done <- err
	}()
	var heldErr error
	require.Eventually(t, func() bool {
		select {
		case err := <-done:
			heldErr = err
			return true
		default:
			return false
		}
	}, 5*time.Second, 5*time.Millisecond,
		"the mid-wait question must break the wait with the refusal, not the cap")
	require.ErrorIs(t, heldErr, ErrBatchAwaitHeldQuestion)
	tl.addf("worker broke on the mid-wait question: err=ErrBatchAwaitHeldQuestion")
}

// TestHeldQuestionIncidentChain is the FULL deterministic incident
// regression: reacted+done notice, wrong resume hint, await refusal,
// correct inject through the bridge, child completes in the SAME
// incarnation, notice closed, await unblocked.
func TestHeldQuestionIncidentChain(t *testing.T) {
	env := newQuestionInstall(t)
	root := env.submitAgentRoot()
	ctx := context.Background()
	tl := newQuestionTestTimeline(t)
	tl.addf("agent root submitted; waiting for the published question")

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

	// 2. The incident's pull+reaction through the REAL production paths
	// (reordered: the coordinator and its node facts binding are built
	// FIRST, because the pull needs the facts binding).
	coord, _ := newControlCoordinator(t)
	coord.sessions = env.sessions
	coord.messages = env.messages
	facts := bindBatchNodeFacts(t, coord, env.conn, t.TempDir())
	pulled, perr := facts.PullPendingSessionNotices(ctx, env.messages, env.sessionID, buildSessionNoticeMessageParams)
	require.NoError(t, perr)
	require.NotEmpty(t, pulled, "the production session-notice pull delivers the child question into history")
	var questionCount int
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE session_id = ?1 AND notice_kind = ?2`, env.sessionID, session.NoticeKindChildQuestion).Scan(&questionCount))
	require.NotZero(t, questionCount, "the question landed in history")
	tl.addf("real pull delivered the question into history (%d notice messages)", questionCount)
	// (b) reaction: a real owner message through the production reaction
	// marking (NOT raw SQL). This is exactly what marked the incident's
	// notice reacted without answering the child.
	msg, merr := env.messages.Create(ctx, env.sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "the owner said something unrelated"}},
	})
	require.NoError(t, merr)
	require.NoError(t, env.store.BatchReactionMarkReactedWithMessageUpdate(ctx, env.messages, env.sessionID, msg))
	var delivery string
	var reacted int64
	var wake int64
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT delivery, reacted, wake FROM session_notices WHERE owner = ?1 AND kind = ?2`,
		env.sessionID, session.NoticeKindChildQuestion).Scan(&delivery, &reacted, &wake))
	require.EqualValues(t, 1, reacted, "the production reaction marked the notice reacted")
	require.Equal(t, "done", delivery, "the pull delivered the notice")
	tl.addf("real reaction: reacted=1 delivery=done")

	// 3. Wrong resume: the refusal text carries the held-question hint with
	// the exact child id, the tool and the node — not a dead end.
	// The resume-error path touches NOTHING from params.Agent (the Get
	// failure returns before any model/driver use), so the zero-value stub
	// only needs to satisfy the SessionAgent interface.
	resp, err := coord.runSubAgent(ctx, subAgentParams{
		SessionID: env.sessionID, Agent: &controlStubAgent{},
		ResumeSessionID: strings.Split(childID, "-")[0],
	})
	require.NoError(t, err)
	require.True(t, resp.IsError, "a bad resume id is a tool error, not a Go error")
	require.Contains(t, resp.Content, "not found", "the original refusal wording survives")
	require.Contains(t, resp.Content, childID, "the hint names the exact FULL child id")
	require.Contains(t, resp.Content, "inject_agent", "the hint names the answer tool")
	require.Contains(t, resp.Content, node, "the hint names the held batch node")

	// 4. Await refusal without an armed sleep. The live reader is bound
	// EXPLICITLY (not via bindBatchCoordinatorControls) because this
	// install's nodes run on the install's own host id, whose lock file no
	// shared probe can classify: the deterministic always-alive probe keeps
	// the capture complete.
	ingress, ierr := NewBatchWorkIngress(env.install.Runtime, NewBatchLeafRegistryStore())
	require.NoError(t, ierr)
	live := session.NewBatchLiveReader(db.New(env.conn), t.TempDir())
	live.IsOwnHost = func(string) bool { return false }
	live.ProbeHost = func(string, string) (session.HostLockStatus, error) {
		return session.HostStatusAlive, nil
	}
	require.NoError(t, BindCoordinatorBatchControls(coord, ingress, nil, live))
	_, err = coord.AwaitTasks(ctx, env.sessionID, "all", 0)
	require.ErrorIs(t, err, ErrBatchAwaitHeldQuestion)
	require.False(t, coord.arb.snapshot(env.sessionID).SleepAll,
		"the old code returned a result and armed the 1h sleep/wake")
	tl.addf("await refused before any sleep (reacted notice still routes)")

	// 5. Correct inject: the reacted=1 notice still routes through the sink.
	sink, serr := NewBatchHeldAnswerSink(env.conn, env.install.Runner)
	require.NoError(t, serr)
	SetCoordinatorBatchHeldAnswerSink(coord, sink)
	t.Cleanup(func() { SetCoordinatorBatchHeldAnswerSink(coord, nil) })
	out, ierr := coord.InjectAgent(ctx, env.sessionID, childID, "yes, proceed", false)
	require.NoError(t, ierr, "the bridge delivers the answer despite reacted=1")
	require.Contains(t, out, "SAME batch execution")
	require.NotContains(t, out, "Message saved", "the legacy mailbox path was not taken")
	tl.addf("sink inject accepted for the SAME incarnation")

	// 6. Child continues in the SAME incarnation; release hygiene.
	var state string
	var doneToken int64
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(ctx,
			`SELECT state, token FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			node, env.sessionID).Scan(&state, &doneToken) == nil && state == "completed"
	}, 5*time.Second, 5*time.Millisecond, "the answered incarnation settles completed")
	require.Equal(t, token, doneToken, "the incarnation token is unchanged")
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(ctx,
			`SELECT state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			string(root), env.sessionID).Scan(&state) == nil && state == "completed"
	}, 5*time.Second, 5*time.Millisecond, "the root settles completed")
	_, _, _, _, _, rawAnswer := env.leaf.snapshot()
	require.Equal(t, "yes, proceed", string(rawAnswer), "the injected answer reached the asker")
	require.Equal(t, 1, env.leaf.runCount(), "exactly ONE launch: no new generation")
	require.NoError(t, env.install.Runner.Release(ctx, root))
	env.releaseConstruction()
	require.Eventually(t, func() bool { return env.install.DiskBindings.ownerCount() == 0 },
		5*time.Second, time.Millisecond, "no provider owner remains")

	// 7. Notice closed: delivery='void', wake=0 — without close-on-acceptance
	// the answered notice would keep refusing every future await forever.
	// (delivery/declared above in step 2; wake likewise.)
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT delivery, wake FROM session_notices WHERE owner = ?1 AND kind = ?2`,
		env.sessionID, session.NoticeKindChildQuestion).Scan(&delivery, &wake))
	require.Equal(t, "void", delivery)
	require.Equal(t, int64(0), wake)

	// 8. Await unblocked: no held-question refusal anymore (the root
	// completed, so "nothing is running" is the expected refusal).
	_, err = coord.AwaitTasks(ctx, env.sessionID, "all", 0)
	require.NotErrorIs(t, err, ErrBatchAwaitHeldQuestion)
	tl.addf("final await unblocked")

	// 9. INSPECT: the answered question no longer promotes the child — no
	// HeldQuestion and not awaiting_answer.
	insp, ierr := coord.InspectAgent(ctx, env.sessionID, childID)
	require.NoError(t, ierr)
	require.Nil(t, insp.HeldQuestion, "the answered question is no longer held")
	require.NotEqual(t, "awaiting_answer", insp.Status, "the answered child is not awaiting_answer")
	tl.addf("inspect: HeldQuestion=nil status=%q", insp.Status)
}

// TestAwaitTasksWorkerRefusesHeldQuestion: the WORKER path (a nested worker
// answering its own children) refuses at the initial evaluation instead of
// blocking in-tool for up to the 40m cap.
// Mutation caught: without the worker-path HeldQuestions dep the call
// blocks until the block cap (res.Blocked after a long wait) instead of
// refusing immediately.
func TestAwaitTasksWorkerRefusesHeldQuestion(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	env, c := newAwaitWorkerEnv(t)
	bindBatchNodeFacts(t, c, env.conn, env.dbDir)
	awtBindControls(t, env, c)
	ctx := context.Background()
	worker := childSession(t, env, "call-held-worker", "parent-held-worker")
	root, _ := awtSeedLiveWork(t, env.conn, worker, "call-held-worker-root")
	// seedBatchDelegation hardcodes token 0 and the publisher refuses a
	// zero token, so give this incarnation a non-zero token on all its rows
	// (the same bump the fence-rejection fixtures use).
	_, uerr := env.conn.ExecContext(ctx, `UPDATE batch_nodes SET token = 77 WHERE root_id = ?1`, root)
	require.NoError(t, uerr)
	var nodeID string
	var token int64
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT node_id, token FROM batch_nodes WHERE root_id = ?1 AND node_id != root_id LIMIT 1`,
		root).Scan(&nodeID, &token))
	require.Equal(t, int64(77), token)
	// The notice OWNER is the worker session (a nested worker answers its
	// own children); the marker's child names the worker's delegation child.
	commitMarkerNotice(t, env.conn, worker, nodeID, worker, token)
	tl.addf("worker-owned question published: node=%s token=%d child=%s", nodeID, token, worker)

	res, err := c.AwaitTasks(ctx, worker, "any", 0)
	require.ErrorIs(t, err, ErrBatchAwaitHeldQuestion,
		"the worker path refuses on a held question instead of blocking")
	require.False(t, res.Blocked, "the refusal fires at the initial evaluation")
	tl.addf("worker await refused at the initial evaluation")
}

// TestWorkerAwaitBreaksOnRealPublicationMidWait: a REAL production
// publication (commitMarkerNotice through the real publisher) that appears
// после начального чтения вопросов прерывает ожидание по hint.
// Poll и deadline исключены; отсутствие повторной проверки завершит context guard.
func TestWorkerAwaitBreaksOnRealPublicationMidWait(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	env, c := newAwaitWorkerEnv(t)
	bindBatchNodeFacts(t, c, env.conn, env.dbDir)
	awtBindControls(t, env, c)
	// Исключаем poll/deadline: после начального чтения сработать должен hint.
	awtShrinkPoll(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	worker := childSession(t, env, "call-midw", "parent-midw")
	root, _ := awtSeedLiveWork(t, env.conn, worker, "call-midw-root")
	// seedBatchDelegation hardcodes token 0 and the publisher refuses a
	// zero token — same as TestAwaitTasksWorkerRefusesHeldQuestion.
	_, uerr := env.conn.ExecContext(ctx, `UPDATE batch_nodes SET token = 77 WHERE root_id = ?1`, root)
	require.NoError(t, uerr)
	var nodeID string
	var token int64
	require.NoError(t, env.conn.QueryRowContext(ctx,
		`SELECT node_id, token FROM batch_nodes WHERE root_id = ?1 AND node_id != root_id LIMIT 1`,
		root).Scan(&nodeID, &token))
	require.Equal(t, int64(77), token)
	tl.addf("live worker work seeded: node=%s token=%d", nodeID, token)
	arrived := make(chan struct{}, 1)
	hook := func(owner, _ string) {
		if owner == worker {
			select {
			case arrived <- struct{}{}:
			default:
			}
		}
	}
	awaitTasksWorkerQuestionReadHook.Store(&hook)
	t.Cleanup(func() { awaitTasksWorkerQuestionReadHook.Store(nil) })
	type awaitResult struct {
		res tools.AwaitTasksResult
		err error
	}
	done := make(chan awaitResult, 1)
	go func() {
		res, err := c.AwaitTasks(ctx, worker, "any", 0)
		done <- awaitResult{res: res, err: err}
	}()
	select {
	case <-arrived:
	case <-ctx.Done():
		t.Fatal("worker did not finish the initial held-question read")
	}
	tl.addf("initial held-question snapshot was empty")
	// NOW publish the REAL question owned by the worker (the worker
	// delegation child is the row childSession created; the established
	// marker-child id is the worker session itself).
	commitMarkerNotice(t, env.conn, worker, nodeID, worker, 77)
	tl.addf("real question published mid-wait: node=%s token=77 child=%s", nodeID, worker)
	c.driverHints.bumpHint(worker)
	select {
	case r := <-done:
		tl.addf("hint evaluation: err=%v blocked=%v timed_out=%v", r.err, r.res.Blocked, r.res.TimedOut)
		require.ErrorIs(t, r.err, ErrBatchAwaitHeldQuestion)
		require.False(t, r.res.Blocked)
		require.False(t, r.res.TimedOut, "deadline cannot substitute for the hint path")
	case <-ctx.Done():
		t.Fatal("published question did not interrupt the hint-driven wait")
	}
	tl.addf("worker broke with ErrBatchAwaitHeldQuestion mid-wait")
}

// TestWorkerAwaitRefusesOnRealHeldChild: the WORKER classification driving
// on a REAL parked leaf child — the questionInstall owner (a real agent-kind
// batch execution parked in a held ask) becomes a durable delegation child,
// and the worker path refuses on ITS held question.
func TestWorkerAwaitRefusesOnRealHeldChild(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	env := newQuestionInstall(t)
	root := env.submitAgentRoot()
	ctx := context.Background()
	var text string
	require.Eventually(t, func() bool {
		return env.conn.QueryRowContext(ctx,
			`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery = 'pending'`,
			env.sessionID, session.NoticeKindChildQuestion).Scan(&text) == nil
	}, 5*time.Second, 5*time.Millisecond, "the held question is published")
	node, tok, ok := parseBatchQuestionMarker(text)
	require.True(t, ok, "the notice carries the production marker: %q", text)
	tl.addf("real parked child question: node=%s token=%d", node, tok)
	// Make the questionInstall OWNER a durable delegation child of a
	// worker parent (ensureFixtureSession inside covers the parent row).
	seedBatchDelegation(t, env.conn, "parent-held-worker-real", "call-worker-real", env.sessionID, "agent", "running", false)
	ensureFixtureSession(t, env.conn, env.sessionID)
	_, parentErr := env.conn.ExecContext(ctx, `UPDATE sessions SET parent_session_id = ? WHERE id = ?`, "parent-held-worker-real", env.sessionID)
	require.NoError(t, parentErr)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = env.install.Runner.Stop(stopCtx, root)
	})
	tl.addf("owner linked to the durable delegation parent")
	coord, _ := newControlCoordinator(t)
	coord.sessions = env.sessions
	coord.messages = env.messages
	bindBatchNodeFacts(t, coord, env.conn, t.TempDir())
	// Explicit controls binding with the always-alive live reader: this
	// install's nodes run on the install's own host id, whose lock file no
	// shared probe can classify (same as TestHeldQuestionIncidentChain).
	ingress, ierr := NewBatchWorkIngress(env.install.Runtime, NewBatchLeafRegistryStore())
	require.NoError(t, ierr)
	live := session.NewBatchLiveReader(db.New(env.conn), t.TempDir())
	live.IsOwnHost = func(string) bool { return false }
	live.ProbeHost = func(string, string) (session.HostLockStatus, error) {
		return session.HostStatusAlive, nil
	}
	require.NoError(t, BindCoordinatorBatchControls(coord, ingress, nil, live))
	isWorker, identityErr := coord.isDurableDelegationChild(ctx, env.sessionID)
	require.NoError(t, identityErr)
	require.True(t, isWorker, "the fixture must actually enter the worker await branch")
	res, err := coord.AwaitTasks(ctx, env.sessionID, "any", 0)
	require.ErrorIs(t, err, ErrBatchAwaitHeldQuestion,
		"the worker path refuses on the REAL parked child's held question")
	require.False(t, res.Blocked, "the refusal fires, not a blocked wait")
	tl.addf("worker await refused on the real held child")
	require.NoError(t, env.install.Runner.Inject(ctx, jobbatch.ID(node), json.RawMessage(`{"depth":1,"sequence":1,"text":"yes, proceed"}`)))
	require.Eventually(t, func() bool {
		view, err := env.install.Runner.View(ctx, root)
		return err == nil && view.State.Terminal()
	}, 10*time.Second, 5*time.Millisecond, "answer must settle the child before Release")
	require.NoError(t, env.install.Runner.Release(ctx, root))
	env.releaseConstruction()
	require.Eventually(t, func() bool { return env.install.DiskBindings.ownerCount() == 0 },
		5*time.Second, time.Millisecond, "no provider owner remains")
}
