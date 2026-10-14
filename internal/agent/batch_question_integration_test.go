package agent

// R11G ruling-3 production integration coverage over a REAL SQLite batch
// store, the REAL inventory runtime (db.Connect, commit hook, real Runner)
// and the REAL agent executor family. Compile-only in this worktree.
//
// Covered here:
//  1. exactly ONE durable child_question notice per (node, token, sequence,
//     depth, child) identity under concurrent publishers, with the exact
//     owner/kind/header metadata and job_tool_call_id NULL;
//  2. a stale-incarnation publish is refused and writes no row;
//  3. an answer injected at a held depth routes into the SAME execution —
//     the asker never exits until the answer arrives, the node reports
//     exactly once and the child output carries the answer;
//  4. the flagged ask wrapper delegates through the trusted-context binding
//     at the stamped depth and falls back to the raw ask tool unbound.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// questionTestNode reserves one live batch root (state running) under the
// env's real store and stamps a non-zero engine-style token so the durable
// publisher's incarnation check has a realistic row to validate against.
func questionTestNode(t *testing.T, env *batchInventoryEnv, callID string) (nodeID string, token int64) {
	t.Helper()
	result, err := env.store.Reserve(env.ctx, session.BatchReservation{
		Owner: env.sessionID, Host: env.install.Runtime.cfg.Host, RunnerID: env.install.Runtime.cfg.RunnerID,
		ToolCallID: callID, InputHash: callID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: sql.NullString{String: "test", Valid: true}, State: "running", ControlState: "none",
		}},
	})
	require.NoError(t, err)
	nodeID = result.Snapshot.Root.RootID
	require.NotEmpty(t, nodeID)
	token = 7
	require.NoError(t, withQuestionToken(env, nodeID, token))
	return nodeID, token
}

func withQuestionToken(env *batchInventoryEnv, nodeID string, token int64) error {
	_, err := env.conn.ExecContext(env.ctx, `UPDATE batch_nodes SET token = ? WHERE root_id = ? AND node_id = ?`, token, nodeID, nodeID)
	return err
}

func countQuestionNotices(t *testing.T, env *batchInventoryEnv, owner string) int {
	t.Helper()
	var n int
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT COUNT(*) FROM session_notices WHERE owner = ? AND kind = ?`,
		owner, session.NoticeKindChildQuestion).Scan(&n))
	return n
}

func TestBatchQuestionDurableNoticeExactlyOnce(t *testing.T) {
	env := newBatchInventoryEnv(t, "agent")
	nodeID, token := questionTestNode(t, env, "q-once")
	child := "child-sess-once"
	publisher := batchCoordinatorQuestionPublisher{store: env.store}
	identity := batchChildQuestionNotice{
		Owner: env.sessionID, ChildSessionID: child, NodeID: nodeID,
		Token: uint64(token), Depth: 2, Sequence: 1, Question: "why?",
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- publisher.PublishChildQuestion(env.ctx, identity) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, countQuestionNotices(t, env, env.sessionID),
		"concurrent identical publishes must produce exactly one durable notice row")
	var owner, kind, text string
	var jobRef sql.NullString
	var wake int
	require.NoError(t, env.conn.QueryRowContext(env.ctx,
		`SELECT owner, kind, text, job_tool_call_id, wake FROM session_notices WHERE owner = ? AND kind = ?`,
		env.sessionID, session.NoticeKindChildQuestion).Scan(&owner, &kind, &text, &jobRef, &wake))
	require.Equal(t, env.sessionID, owner)
	require.Equal(t, session.NoticeKindChildQuestion, kind)
	require.False(t, jobRef.Valid, "batch question notices must not borrow the async_jobs identity")
	require.Equal(t, 1, wake, "a held question must wake the owner session")
	require.True(t, strings.HasPrefix(text,
		"[batch-question node="+nodeID+" token=7 sequence=1 depth=2 child="+child+"]\n"),
		"notice text must carry the exact stable identity header, got %q", text)
}

func TestBatchQuestionRefusesStaleIncarnation(t *testing.T) {
	env := newBatchInventoryEnv(t, "agent")
	nodeID, token := questionTestNode(t, env, "q-stale")
	publisher := batchCoordinatorQuestionPublisher{store: env.store}
	err := publisher.PublishChildQuestion(env.ctx, batchChildQuestionNotice{
		Owner: env.sessionID, ChildSessionID: "child-stale", NodeID: nodeID,
		Token: uint64(token) + 1, Depth: 1, Sequence: 1, Question: "still there?",
	})
	require.Error(t, err, "a publish naming a non-current incarnation token must be refused")
	require.Equal(t, 0, countQuestionNotices(t, env, env.sessionID),
		"a refused stale publish must leave no durable row")
}

// TestBatchQuestionAnswerRoutesSameExecution proves ruling 3's core: the
// answer travels into the SAME incarnation through the executor's own
// Messenger seam, the asker goroutine never exits until it arrives, and the
// node settles exactly once with the answer as the child output.
func TestBatchQuestionAnswerRoutesSameExecution(t *testing.T) {
	turn := newBatchAgentTestTurn(batchAgentTurnResult{}, nil)
	factory := newBatchAgentTestFactory(turn)
	factory.useTurn = func(t2 *batchAgentTestTurn, host batchAgentHost) {
		answer, err := host.Ask(context.Background(), 3, "why?")
		if err != nil {
			t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextErrorResponse(err.Error())}, nil)
		} else {
			t2.setRes(batchAgentTurnResult{Response: fantasy.NewTextResponse(string(answer))}, nil)
		}
		t2.finish()
		t2.exitNow()
	}
	var execPtr atomic.Pointer[batchAgentExecution]
	var answeredNode atomic.Value
	answeredNode.Store("")
	deps := batchAgentDeps{}
	deps.InjectQuestionAnswer = func(_ context.Context, node string, depth int, answer json.RawMessage) error {
		exec := execPtr.Load()
		if exec == nil {
			t.Error("answer routed before the execution existed")
			return ErrBatchAgentNotHeld
		}
		require.Equal(t, string(exec.requestSnapshot().NodeID), node,
			"the answer must be routed to the SAME node identity the question was published under")
		require.Equal(t, 3, depth)
		answeredNode.Store(node)
		return jobbatch.Messenger(exec).Inject(context.Background(),
			json.RawMessage(`{"depth":3,"text":"because"}`))
	}
	e, factory, reader, reports := batchAgentTestRun(t, deps, factory)
	execPtr.Store(e)
	require.Eventually(t, func() bool {
		_, held := e.heldDepth()
		return held
	}, 2*time.Second, time.Millisecond)
	depth, _ := e.heldDepth()
	require.Equal(t, 3, depth)
	// While held with terminal descendants the node must NOT settle.
	reader.set(false, true)
	batchAgentTestNoReport(t, reports, "a question is held")
	require.NoError(t, e.answerQuestion(context.Background(), 3, json.RawMessage(`"because"`)))
	reader.set(false, true)
	r := batchAgentTestReport(t, reports)
	require.Equal(t, jobbatch.StateCompleted, r.State)
	require.Contains(t, r.Summary, "because")
	require.Equal(t, string(e.requestSnapshot().NodeID), answeredNode.Load().(string),
		"the routed answer must name this execution's own node")
	require.ErrorIs(t, jobbatch.Messenger(e).Inject(context.Background(),
		json.RawMessage(`{"depth":3,"text":"late"}`)), ErrBatchAgentStaleGeneration,
		"a terminal incarnation must reject further answers as stale")
}

// TestBatchQuestionWrapperDelegatesAtStampedDepth proves the flagged ask
// wrapper reads its trusted context: bound, it routes through the host ask
// with the stamped depth and shapes the answer as a successful tool
// response; unbound, it delegates straight to the raw ask tool.
func TestBatchQuestionWrapperDelegatesAtStampedDepth(t *testing.T) {
	raw := tools.NewAskQuestionTool()
	wrapper := batchAgentAskQuestionTool{AgentTool: raw}
	type askCall struct {
		depth    int
		question string
	}
	calls := make(chan askCall, 1)
	binding := batchAgentQuestionBinding{Ask: func(_ context.Context, depth int, question string) (json.RawMessage, error) {
		calls <- askCall{depth: depth, question: question}
		return json.RawMessage(`"因为"`), nil
	}}
	ctx := withBatchAgentQuestionContext(withBatchAgentQuestionBinding(context.Background(), binding), 4)
	resp, err := wrapper.Run(ctx, fantasy.ToolCall{ID: "ask-1", Name: tools.AskQuestionToolName, Input: `{"question":"why?"}`})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "因为", resp.Content)
	select {
	case got := <-calls:
		require.Equal(t, 4, got.depth, "the wrapper must ask at the stamped trusted depth")
		require.Equal(t, "why?", got.question)
	default:
		t.Fatal("the bound wrapper never routed through the host ask")
	}

	_, unboundErr := wrapper.Run(context.Background(), fantasy.ToolCall{ID: "ask-2", Name: tools.AskQuestionToolName, Input: `{"question":"why?"}`})
	require.Error(t, unboundErr,
		"the unbound wrapper delegates to the raw ask tool, which stops the turn awaiting an answer")
}

// TestBatchQuestionPublishFailsClosedWithoutIdentity: the durable publisher
// refuses an incomplete identity instead of writing a partial row.
func TestBatchQuestionPublishFailsClosedWithoutIdentity(t *testing.T) {
	env := newBatchInventoryEnv(t, "agent")
	publisher := batchCoordinatorQuestionPublisher{store: env.store}
	require.Error(t, publisher.PublishChildQuestion(env.ctx, batchChildQuestionNotice{
		Owner: env.sessionID, NodeID: "root", Token: 1, Depth: 1, Sequence: 1,
	}))
	require.Equal(t, 0, countQuestionNotices(t, env, env.sessionID))
}
