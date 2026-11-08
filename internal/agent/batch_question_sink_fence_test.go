package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestBatchQuestionSinkRejectsRetiredRoot(t *testing.T) {
	_, conn, _ := newTestBatchStore(t)
	ctx := context.Background()
	const owner, child = "retired-question-owner", "retired-question-child"
	createSessionRow(t, db.New(conn), ctx, owner)
	createSessionRow(t, db.New(conn), ctx, child)
	root, node := seedDelegation(t, conn, owner, "retired-question-call", child, 7)
	commitMarkerNotice(t, conn, owner, node, child, 7)
	_, err := conn.ExecContext(ctx, `UPDATE batch_nodes SET delivery = 'void' WHERE node_id = ?1`, root)
	require.NoError(t, err)
	runner := &bridgeFakeRunner{}
	sink, err := NewBatchHeldAnswerSink(conn, runner)
	require.NoError(t, err)
	_, err = sink.DeliverHeldAnswer(ctx, owner, child, "answer")
	require.ErrorIs(t, err, ErrBatchAgentStaleGeneration)
	require.Zero(t, runner.count())
}

func TestBatchQuestionSinkClosesOnlyAnsweredIncarnation(t *testing.T) {
	_, conn, _ := newTestBatchStore(t)
	ctx := context.Background()
	const owner, child = "scoped-question-owner", "scoped-question-child"
	createSessionRow(t, db.New(conn), ctx, owner)
	createSessionRow(t, db.New(conn), ctx, child)
	_, first := seedDelegation(t, conn, owner, "scoped-question-first", child, 7)
	commitMarkerNotice(t, conn, owner, first, child, 7)
	_, second := seedDelegation(t, conn, owner, "scoped-question-second", child, 8)
	commitMarkerNotice(t, conn, owner, second, child, 8)
	runner := &bridgeFakeRunner{}
	sink, err := NewBatchHeldAnswerSink(conn, runner)
	require.NoError(t, err)
	fact, err := sink.DeliverHeldAnswer(ctx, owner, child, "answer")
	require.NoError(t, err)
	require.Equal(t, second, fact.NodeID)
	require.Equal(t, 1, runner.count())
	for node, wantWake := range map[string]int{first: 1, second: 0} {
		var wake int
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT wake FROM session_notices WHERE owner = ?1 AND instr(text, ?2) > 0`, owner, node).Scan(&wake))
		require.Equal(t, wantWake, wake, "notice for node %s", node)
	}
}
