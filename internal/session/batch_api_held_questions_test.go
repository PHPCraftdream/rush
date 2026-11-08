package session

// Regression for the API held-question listing invariant: the listing shows
// exactly the CURRENT-incarnation unresolved child questions regardless of
// `reacted` (the owner's reaction flow marks notices reacted without
// answering), ANSWERED questions disappear through delivery='void' (the
// resolution lifecycle), and the per-candidate incarnation + root fence
// hides stale-token and retired-root questions. An unreadable fence read
// (cancelled ctx) is conservative: parsed prefix plus non-nil error.
//
// Mutation caught: restoring `AND reacted = 0` makes step 2 drop the
// incident state from the listing; dropping the token fence or the
// root-void term makes steps 4/5 list dead questions; removing
// BatchResolveChildQuestions from the resolution path leaves answered
// questions listed forever.

import (
	"context"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func batchAPIHeldSeed(t *testing.T, s *BatchStore, ctx context.Context, owner, callID string, token int64) (node string) {
	t.Helper()
	res, err := s.Reserve(ctx, BatchReservation{
		Owner: owner, Host: "host-api", RunnerID: "runner-api", ToolCallID: callID,
		InputHash: "hash-" + callID, Sequence: 1,
		Root: BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: batchStoreTestString("parallel"), State: "running", ControlState: "none", Token: token,
		}, Children: []BatchReservationNode{{Values: db.ReserveBatchNodeParams{
			Kind: "task", TaskKind: batchStoreTestString("agent"), State: "running", ControlState: "none", Token: token,
		}}}},
	})
	require.NoError(t, err)
	return res.Snapshot.Nodes[1].NodeID
}

func batchAPIHeldPublish(t *testing.T, s *BatchStore, ctx context.Context, owner, node, child string, token, sequence int64) {
	t.Helper()
	marker := fmt.Sprintf("[batch-question node=%s token=%d sequence=%d depth=%d child=%s]\n", node, token, sequence, 1, child)
	text := marker + "SUB-AGENT QUESTION: which one?\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	require.NoError(t, s.BatchPublishQuestion(ctx, owner, node, token, marker, text))
}

func TestBatchAPIHeldQuestionsLifecycleAndFences(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	const owner = "owner"
	const child = "owner-child-q"
	require.NoError(t, seedSession(ctx, db.New(conn), child))

	node := batchAPIHeldSeed(t, s, ctx, owner, "call-api-1", 7)
	batchAPIHeldPublish(t, s, ctx, owner, node, child, 7, 1)

	// 1. A live fenced question is listed with its parsed identity.
	listed, err := s.BatchAPIHeldQuestions(ctx, owner)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, node, listed[0].Node)
	require.Equal(t, child, listed[0].Child)
	require.EqualValues(t, 7, listed[0].Token)
	require.EqualValues(t, 1, listed[0].Sequence)
	require.Contains(t, listed[0].Question, "which one?")

	// 2. The incident state (delivered + reacted) STAYS listed: reacted is
	// not answered.
	_, err = conn.ExecContext(ctx,
		`UPDATE session_notices SET delivery='done', reacted=1 WHERE owner = ?1 AND kind = ?2`,
		owner, NoticeKindChildQuestion)
	require.NoError(t, err)
	listed, err = s.BatchAPIHeldQuestions(ctx, owner)
	require.NoError(t, err)
	require.Len(t, listed, 1, "a reacted-but-unanswered question is still listed")

	// 3. Resolution closes the question: not listed afterwards, the closed
	// count is 1, and a second resolve is a no-op.
	closed, err := s.BatchResolveChildQuestions(ctx, owner, node, child, 7, 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, closed)
	listed, err = s.BatchAPIHeldQuestions(ctx, owner)
	require.NoError(t, err)
	require.Empty(t, listed, "an answered (resolved) question is not listed")
	closed, err = s.BatchResolveChildQuestions(ctx, owner, node, child, 7, 1)
	require.NoError(t, err)
	require.Zero(t, closed, "a second resolution is a no-op")

	// 4. A stale incarnation (token bumped) is not listed.
	node2 := batchAPIHeldSeed(t, s, ctx, owner, "call-api-2", 8)
	batchAPIHeldPublish(t, s, ctx, owner, node2, child, 8, 1)
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET token = 9 WHERE node_id = ?1`, node2)
	require.NoError(t, err)
	listed, err = s.BatchAPIHeldQuestions(ctx, owner)
	require.NoError(t, err)
	require.Empty(t, listed, "a stale-token question is not listed")

	// 5. A retired ROOT (delivery='void') hides its still-live child's
	// question.
	node3 := batchAPIHeldSeed(t, s, ctx, owner, "call-api-3", 10)
	batchAPIHeldPublish(t, s, ctx, owner, node3, child, 10, 1)
	var root3 string
	require.NoError(t, conn.QueryRowContext(ctx,
		`SELECT root_id FROM batch_nodes WHERE node_id = ?1`, node3).Scan(&root3))
	_, err = conn.ExecContext(ctx, `UPDATE batch_nodes SET delivery='void', wake=0 WHERE node_id = ?1`, root3)
	require.NoError(t, err)
	listed, err = s.BatchAPIHeldQuestions(ctx, owner)
	require.NoError(t, err)
	require.Empty(t, listed, "a question of a retired root is not listed")

	// 6. An unreadable fence read (cancelled ctx) is conservative: parsed
	// prefix plus non-nil error.
	cctx, ccancel := context.WithCancel(ctx)
	ccancel()
	listed, err = s.BatchAPIHeldQuestions(cctx, owner)
	require.Error(t, err, "an unreadable fence read must not read as proven absence")
	require.Empty(t, listed, "the display consumer degrades to an empty listing for that call")
}

func TestBatchAPIHeldQuestionDeletedNodeIsNotReadFailure(t *testing.T) {
	s, conn, ctx := batchStoreTestDB(t)
	node := batchAPIHeldSeed(t, s, ctx, "owner", "deleted-question-node", 7)
	batchAPIHeldPublish(t, s, ctx, "owner", node, "former-child", 7, 1)
	_, err := conn.ExecContext(ctx, `DELETE FROM batch_nodes WHERE node_id = ?`, node)
	require.NoError(t, err)
	listed, err := s.BatchAPIHeldQuestions(ctx, "owner")
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("deleted node=%s list_error=%v listed=%d", node, err, len(listed))
		}
	})
	require.NoError(t, err, "a missing incarnation cannot own an actionable question")
	require.Empty(t, listed)
}
