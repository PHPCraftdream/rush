package agent

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type busyQuestionSessionAgent struct{ SessionAgent }

func (busyQuestionSessionAgent) IsSessionBusy(string) bool { return true }

func busyQuestionDriver(coord *coordinator, child string) *busyQuestionSessionAgent {
	driver := &busyQuestionSessionAgent{}
	coord.subAgentDrivers.register(child, subAgentDriver{agent: driver})
	return driver
}

func TestCLIActiveWorkBatch_BusyHeldQuestionDormancy(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-busy-question-owner"
	const child = "sess-busy-question-child"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, child)
	_, nodeID := seedDelegation(t, conn, owner, "call-busy-question", child, 81)
	commitMarkerNotice(t, conn, owner, nodeID, child, 81)
	messages, err := coord.messages.List(ctx, child)
	require.NoError(t, err)
	require.Empty(t, messages, "busy held question has no finished assistant metadata")
	driver := busyQuestionDriver(coord, child)
	require.Same(t, driver, coord.agentFor(child))
	require.True(t, coord.agentFor(child).IsSessionBusy(child))
	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.False(t, active, "the current committed held marker makes the busy question child dormant")
}

func TestCLIActiveWorkBatch_BusyHeldQuestionRequiresNoticeAndFence(t *testing.T) {
	t.Run("missing notice", func(t *testing.T) {
		coord, _, conn, _, ctx := batchQuestionFixture(t)
		const owner = "sess-busy-missing-owner"
		const child = "sess-busy-missing-child"
		q := db.New(conn)
		createSessionRow(t, q, ctx, owner)
		createSessionRow(t, q, ctx, child)
		seedDelegation(t, conn, owner, "call-busy-missing", child, 82)
		busyQuestionDriver(coord, child)
		active, err := coord.CLIActiveWork(ctx, owner)
		require.NoError(t, err)
		require.True(t, active, "without a retained marker, busy work stays active")
	})

	t.Run("stale token", func(t *testing.T) {
		coord, _, conn, _, ctx := batchQuestionFixture(t)
		const owner = "sess-busy-stale-owner"
		const child = "sess-busy-stale-child"
		q := db.New(conn)
		createSessionRow(t, q, ctx, owner)
		createSessionRow(t, q, ctx, child)
		_, nodeID := seedDelegation(t, conn, owner, "call-busy-stale", child, 83)
		insertFencedNotice(t, conn, owner, nodeID, child, 84)
		busyQuestionDriver(coord, child)
		active, err := coord.CLIActiveWork(ctx, owner)
		require.NoError(t, err)
		require.True(t, active, "a stale token cannot make the busy child dormant")
	})

	t.Run("wrong owner", func(t *testing.T) {
		coord, _, conn, _, ctx := batchQuestionFixture(t)
		const owner = "sess-busy-wrong-owner"
		const other = "sess-busy-other-owner"
		const child = "sess-busy-wrong-owner-child"
		q := db.New(conn)
		createSessionRow(t, q, ctx, owner)
		createSessionRow(t, q, ctx, other)
		createSessionRow(t, q, ctx, child)
		_, nodeID := seedDelegation(t, conn, owner, "call-busy-wrong-owner", child, 85)
		insertFencedNotice(t, conn, other, nodeID, child, 85)
		busyQuestionDriver(coord, child)
		active, err := coord.CLIActiveWork(ctx, owner)
		require.NoError(t, err)
		require.True(t, active, "a held marker under another owner cannot release this scope")
	})
}

func TestCLIActiveWorkBatch_BusyHeldQuestionCommandSiblingKeepsActive(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-busy-question-sibling-owner"
	const child = "sess-busy-question-sibling-child"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, child)
	_, nodeID := seedMixedRoot(t, conn, owner, "call-busy-question-sibling", child, 86, "running")
	commitMarkerNotice(t, conn, owner, nodeID, child, 86)
	busyQuestionDriver(coord, child)
	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, active, "the command sibling remains active beside the held question")
}

func TestCLIActiveWorkBatch_BusyHeldQuestionNestedCommandKeepsActive(t *testing.T) {
	coord, _, conn, _, ctx := batchQuestionFixture(t)
	const owner = "sess-busy-question-desc-owner"
	const child = "sess-busy-question-desc-child"
	q := db.New(conn)
	createSessionRow(t, q, ctx, owner)
	createSessionRow(t, q, ctx, child)
	_, parentNodeID := seedDelegation(t, conn, owner, "call-busy-question-desc", child, 87)
	commitMarkerNotice(t, conn, owner, parentNodeID, child, 87)
	seedBatchDelegation(t, conn, child, "call-busy-question-desc-command", "", "command", "running", false)
	busyQuestionDriver(coord, child)
	active, err := coord.CLIActiveWork(ctx, owner)
	require.NoError(t, err)
	require.True(t, active, "the descendant command remains live under the held-question child")
}

var _ SessionAgent = (*busyQuestionSessionAgent)(nil)

func insertFencedNotice(t *testing.T, conn *sql.DB, owner, nodeID, child string, token int64) {
	t.Helper()
	marker := fmt.Sprintf("[batch-question node=%s token=%d sequence=1 depth=1 child=%s]\n", nodeID, token, child)
	text := marker + "SUB-AGENT QUESTION: which one?\nAnswer with inject_agent for this existing batch node; do not resume the delegation as a new generation."
	_, err := conn.ExecContext(t.Context(), `INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at) VALUES (?, ?, ?, 1, 'pending', NULL, ?, ?)`, owner, session.NoticeKindChildQuestion, text, time.Now().Unix(), time.Now().Unix())
	require.NoError(t, err)
}
