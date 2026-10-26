package session

// R12-EF fix F3: the question channel of a rerun-retired root is dead. The
// reconcile voids pending/done child_question notices whose marker names a
// node of a void-delivery root, and BatchPublishQuestion refuses to publish
// onto a retired root's subtree -- so the new branch can neither receive nor
// keep receiving questions of the old one.

import (
	"strconv"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func (f *batchRerunFx) setToken(nodeID string, token int64) {
	f.t.Helper()
	_, err := f.conn.ExecContext(f.ctx, `UPDATE batch_nodes SET token = ? WHERE node_id = ?`, token, nodeID)
	require.NoError(f.t, err)
}

func (f *batchRerunFx) publishQuestion(nodeID string, token int64) error {
	marker := "[batch-question node=" + nodeID + " token=" + strconv.FormatInt(token, 10) + " sequence=1 depth=1 child=child-1]\n"
	return f.store.BatchPublishQuestion(f.ctx, batchRerunOwner, nodeID, token, marker, marker+"SUB-AGENT QUESTION: which one?")
}

// questionDelivery reads the delivery/wake of the one child_question notice
// whose marker names nodeID (node ids are UUIDs: no LIKE metacharacters).
func (f *batchRerunFx) questionDelivery(nodeID string) (string, int64) {
	f.t.Helper()
	var delivery string
	var wake int64
	require.NoError(f.t, f.conn.QueryRowContext(f.ctx,
		`SELECT delivery, wake FROM session_notices WHERE owner = ? AND kind = ? AND text LIKE '[batch-question node=`+nodeID+` %'`,
		batchRerunOwner, NoticeKindChildQuestion).Scan(&delivery, &wake))
	return delivery, wake
}

// TestBatchRerunReconcile_VoidsChildQuestionsOfRetiredRoot: the rerun that
// retires a root voids its held child_question notice, and only its notice
// -- a question of a root the rerun keeps stays deliverable.
//
// REVERT CHECK (F3 void): remove the batchRerunVoidChildQuestionsOfVoidRoots
// call from BatchRerunReconcile (internal/session/batch_history_rerun.go).
// The first failing assertion is the retired question's delivery check: it
// stays 'pending' with wake=1, so the next pull delivers a question of a
// root the new branch can never answer.
func TestBatchRerunReconcile_VoidsChildQuestionsOfRetiredRoot(t *testing.T) {
	t.Parallel()
	f := newBatchRerunFx(t)

	target := f.add(message.User, message.TextContent{Text: "rerun me"})
	asst := f.assistantCall("call-1")
	old := f.reserve("call-1")
	f.start(old)
	announce := f.ack(old)
	f.setToken(old.Nodes[1].NodeID, 7)
	require.NoError(t, f.publishQuestion(old.Nodes[1].NodeID, 7), "the live incarnation publishes its question")

	// Negative control: a question of a root the rerun KEEPS (its announce
	// message survives the truncation) must stay live.
	kept := f.reserve("call-kept")
	f.start(kept)
	f.ack(kept)
	f.setToken(kept.Nodes[1].NodeID, 9)
	require.NoError(t, f.publishQuestion(kept.Nodes[1].NodeID, 9))

	res, err := f.reconcile(target.ID, asst.ID, announce.ID)
	require.NoError(t, err)
	require.Len(t, res.Voided, 1)
	require.Equal(t, "void", f.root(old.Root.RootID).Delivery, "precondition: the rerun retired the old root")

	delivery, wake := f.questionDelivery(old.Nodes[1].NodeID)
	require.Equal(t, "void", delivery, "the retired root's held question is voided by the rerun (F3)")
	require.Zero(t, wake, "the voided question wakes nothing")

	keptDelivery, keptWake := f.questionDelivery(kept.Nodes[1].NodeID)
	require.Equal(t, "pending", keptDelivery, "a question of a surviving root stays deliverable")
	require.EqualValues(t, 1, keptWake, "the surviving root's question keeps its wake")
}

// TestBatchPublishQuestion_RefusesRetiredRoot: after the reconcile retired
// the root, a still-live child (stop not yet delivered, or a foreign host)
// cannot publish a question onto the retired subtree.
//
// REVERT CHECK (F3 gate): remove the root-void refusal from
// BatchPublishQuestion (internal/session/batch_question.go). The
// require.Error below FAILS: the publish passes the token/live-state fence
// (the void write never touches state) and dedup-commits as a silent no-op,
// reporting success for a question the new branch could never answer.
func TestBatchPublishQuestion_RefusesRetiredRoot(t *testing.T) {
	t.Parallel()
	f := newBatchRerunFx(t)

	target := f.add(message.User, message.TextContent{Text: "rerun me"})
	asst := f.assistantCall("call-1")
	old := f.reserve("call-1")
	f.start(old)
	announce := f.ack(old)
	f.setToken(old.Nodes[1].NodeID, 7)
	require.NoError(t, f.publishQuestion(old.Nodes[1].NodeID, 7), "the live incarnation publishes its question")

	_, err := f.reconcile(target.ID, asst.ID, announce.ID)
	require.NoError(t, err)
	require.Equal(t, "void", f.root(old.Root.RootID).Delivery, "precondition: the root is durably retired")

	require.Error(t, f.publishQuestion(old.Nodes[1].NodeID, 7),
		"publishing onto a retired root's subtree is refused (F3)")

	var count int64
	require.NoError(t, f.conn.QueryRowContext(f.ctx,
		`SELECT count(*) FROM session_notices WHERE owner = ? AND kind = ?`,
		batchRerunOwner, NoticeKindChildQuestion).Scan(&count))
	require.EqualValues(t, 1, count, "only the pre-retirement question exists: the refused publish inserted nothing")
}
