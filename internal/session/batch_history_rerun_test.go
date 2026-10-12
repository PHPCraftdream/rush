// BatchRerunReconcile coverage (R10c, D-R10c-3/4, I5): a rerun of a call
// reconciles node rows against exactly the history it deleted, and every
// late completion/ack/stop of the OLD incarnation -- same tool_call_id,
// different root_id -- can never reach, settle or ack the NEW one. Real
// SQLite.
package session

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

const batchRerunOwner = "rerun-owner"

type batchRerunFx struct {
	t        *testing.T
	store    *BatchStore
	conn     *sql.DB
	ctx      context.Context
	messages message.Service
	seq      int64
}

func newBatchRerunFx(t *testing.T) *batchRerunFx {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.ReleaseConn(conn)) })
	require.NoError(t, seedSession(ctx, db.New(conn), batchRerunOwner))
	return &batchRerunFx{t: t, store: NewBatchStore(conn), conn: conn, ctx: ctx, messages: message.NewService(db.New(conn)), seq: 1}
}

func (f *batchRerunFx) q() *db.Queries { return db.New(f.conn) }

func (f *batchRerunFx) nextSeq() int64 { f.seq++; return f.seq }

func (f *batchRerunFx) add(role message.MessageRole, parts ...message.ContentPart) message.Message {
	f.t.Helper()
	m, err := f.messages.Create(f.ctx, batchRerunOwner, message.CreateMessageParams{Role: role, Parts: parts})
	require.NoError(f.t, err)
	return m
}

func (f *batchRerunFx) assistantCall(callID string) message.Message {
	return f.add(message.Assistant, message.ToolCall{ID: callID, Name: "bash", Input: "{}", Finished: true})
}

// reserve creates a fresh root incarnation for callID with one task.
func (f *batchRerunFx) reserve(callID string) BatchSnapshot {
	f.t.Helper()
	f.seq++
	p := batchStoreTestReservation()
	p.Owner, p.ToolCallID, p.Sequence, p.InputHash = batchRerunOwner, callID, f.seq, "hash-"+callID
	r, err := f.store.Reserve(f.ctx, p)
	require.NoError(f.t, err)
	return r.Snapshot
}

// start projects the root and its task to running.
func (f *batchRerunFx) start(snap BatchSnapshot) {
	f.t.Helper()
	require.NoError(f.t, f.store.Project(f.ctx, BatchProjection{Host: "host", Sequence: f.nextSeq(), Roots: []BatchRootProjection{{Owner: batchRerunOwner, Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{
		batchStoreTestUpdate(snap.Root.NodeID, "running"), batchStoreTestUpdate(snap.Nodes[1].NodeID, "running"),
	}}}}))
}

// ack names the root's announce ("started") message.
func (f *batchRerunFx) ack(snap BatchSnapshot) message.Message {
	f.t.Helper()
	msg, err := f.store.BatchAnnounceStarted(f.ctx, f.messages, batchRerunOwner, snap.Root.RootID, message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: snap.Root.ToolCallID.String, Name: "bash", Content: "started"}},
	})
	require.NoError(f.t, err)
	return msg
}

// settle completes the task and the root, selects both notices and pulls
// them into history (the root row is then notice_selected=1, delivery done,
// state completed -- notice selection requires a terminal node).
func (f *batchRerunFx) settle(snap BatchSnapshot) (taskNotice, rootNotice message.Message) {
	f.t.Helper()
	task := batchStoreTestNotice(snap.Nodes[1].NodeID)
	root := batchStoreTestNotice(snap.Root.NodeID)
	root.NoticeKind = batchStoreTestString("root")
	require.NoError(f.t, f.store.Project(f.ctx, BatchProjection{Host: "host", Sequence: f.nextSeq(), Roots: []BatchRootProjection{{Owner: batchRerunOwner, Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{
		batchStoreTestUpdate(snap.Nodes[1].NodeID, "completed"), batchStoreTestUpdate(snap.Root.NodeID, "completed"),
	}, Notices: []db.SelectBatchNodeNoticeParams{task, root}}}}))
	pulled, err := f.store.BatchPullNotices(f.ctx, f.messages, batchRerunOwner, func(row BatchNoticeRow) message.CreateMessageParams {
		return message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "notice:" + row.NodeID}}}
	})
	require.NoError(f.t, err)
	require.Len(f.t, pulled, 2)
	return pulled[0].Message, pulled[1].Message
}

// selectOnlyNotice completes the node and selects its notice without
// pulling it (delivery pending, announce message untouched).
func (f *batchRerunFx) selectNotice(snap BatchSnapshot, nodeID, kind string) {
	f.t.Helper()
	n := batchStoreTestNotice(nodeID)
	n.NoticeKind = batchStoreTestString(kind)
	require.NoError(f.t, f.store.Project(f.ctx, BatchProjection{Host: "host", Sequence: f.nextSeq(), Roots: []BatchRootProjection{{Owner: batchRerunOwner, Root: snap.Root.RootID, Updates: []db.ProjectBatchNodeParams{
		batchStoreTestUpdate(nodeID, "completed"),
	}, Notices: []db.SelectBatchNodeNoticeParams{n}}}}))
}

func (f *batchRerunFx) root(rootID string) db.BatchNode {
	f.t.Helper()
	r, err := f.q().GetBatchRoot(f.ctx, db.GetBatchRootParams{RootID: rootID, OwnerSessionID: batchRerunOwner})
	require.NoError(f.t, err)
	return r
}

func (f *batchRerunFx) node(rootID, nodeID string) db.BatchNode {
	f.t.Helper()
	n, err := f.q().GetBatchNode(f.ctx, db.GetBatchNodeParams{RootID: rootID, NodeID: nodeID, OwnerSessionID: batchRerunOwner})
	require.NoError(f.t, err)
	return n
}

func (f *batchRerunFx) reconcile(target string, tail ...string) (BatchRerunTruncation, error) {
	return f.store.BatchRerunReconcile(f.ctx, f.messages, RerunTruncateParams{Owner: batchRerunOwner, TargetID: target, TailIDs: tail})
}

// Revert-checks (e) (f) (g), run one at a time:
//
//	(e) removing `announce_message_id IS NULL` from
//	    BatchRerunVoidRootsByToolCallIDs lets the reused call id of rerun #2
//	    void the NEW incarnation -- the emptiness/delivery assertions fail;
//	(f) attaching Reserve to terminal roots (GetLiveBatchRoot losing its
//	    live-state predicate) reuses the old root_id -- the NotEqual fails;
//	(g) widening the Voided set to a tool_call_id lookup adds the new
//	    incarnation to Voided -- the emptiness assertion fails.
func TestBatchRerunNewIncarnationFencedFromLateOldIncarnation(t *testing.T) {
	t.Parallel()
	f := newBatchRerunFx(t)

	target := f.add(message.User, message.TextContent{Text: "rerun me"})
	asst := f.assistantCall("call-1")
	old := f.reserve("call-1")
	f.start(old)
	announce := f.ack(old)
	taskNotice, rootNotice := f.settle(old)

	res, err := f.reconcile(target.ID, asst.ID, announce.ID, taskNotice.ID, rootNotice.ID)
	require.NoError(t, err)
	require.Len(t, res.Deleted, 5)
	require.Len(t, res.Voided, 1, "the acked old incarnation is voided: %v", res.Voided)
	require.Equal(t, old.Root.RootID, res.Voided[0].RootID, "stop routing carries the exact root_id")
	require.Equal(t, old.Root.NodeID, res.Voided[0].NodeID)
	require.Equal(t, "completed", res.Voided[0].State)
	require.Equal(t, "void", f.root(old.Root.RootID).Delivery)
	require.Equal(t, "void", f.node(old.Root.RootID, old.Nodes[1].NodeID).Delivery, "a node of a voided root never re-pends")

	// The old incarnation is terminal, so the unique live key index permits
	// the new incarnation's reservation under the same tool_call_id.

	// The rerun creates a NEW root incarnation under the same tool_call_id.
	re := f.reserve("call-1")
	require.NotEqual(t, old.Root.RootID, re.Root.RootID, "a rerun gets a fresh incarnation")
	require.Equal(t, "call-1", re.Root.ToolCallID.String)
	f.start(re)
	f.ack(re)
	f.settle(re)
	before, err := f.store.Read(f.ctx, batchRerunOwner, re.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, "done", before.Root.Delivery)

	// A late COMPLETION of the OLD incarnation keys its own rows only: it
	// may rewrite them, but it can never reach the new incarnation. (A
	// second notice selection is refused by the R5 guard: the node already
	// carries the rerun's selected notice.)
	late := BatchProjection{Host: "host", Sequence: f.nextSeq(), Roots: []BatchRootProjection{{Owner: batchRerunOwner, Root: old.Root.RootID, Updates: []db.ProjectBatchNodeParams{
		batchStoreTestUpdate(old.Nodes[1].NodeID, "completed"),
	}}}}
	require.NoError(t, f.store.Project(f.ctx, late))
	lateNotice := BatchProjection{Host: "host", Sequence: f.nextSeq(), Roots: []BatchRootProjection{{Owner: batchRerunOwner, Root: old.Root.RootID, Updates: []db.ProjectBatchNodeParams{
		batchStoreTestUpdate(old.Nodes[1].NodeID, "completed"),
	}, Notices: []db.SelectBatchNodeNoticeParams{batchStoreTestNotice(old.Nodes[1].NodeID)}}}}
	require.ErrorIs(t, f.store.Project(f.ctx, lateNotice), ErrBatchProjectionConflict)

	// A late ACK of the old incarnation elects nothing (already announced).
	_, err = f.store.BatchAnnounceStarted(f.ctx, f.messages, batchRerunOwner, old.Root.RootID, message.CreateMessageParams{
		Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "call-1", Name: "bash", Content: "late"}},
	})
	require.ErrorIs(t, err, ErrBatchAnnouncementLost)

	// A late STOP of the old incarnation targets the terminal root only.
	_, err = f.store.BatchRequestStop(f.ctx, BatchControlTarget{Owner: batchRerunOwner, Host: "host", Root: old.Root.RootID, Node: old.Nodes[1].NodeID}, func(string) (HostLockStatus, error) { return HostStatusAlive, nil })
	require.ErrorIs(t, err, ErrBatchControlStale)
	controls, err := f.store.PendingControls(f.ctx, batchRerunOwner, "host", re.Root.RootID)
	require.NoError(t, err)
	require.Empty(t, controls, "no old-incarnation stop may target the new incarnation")

	// Rerun #2 reuses the call id in its tail (provider numbering): the NEW
	// incarnation already acked, so the legacy arm must never reach it.
	target2 := f.add(message.User, message.TextContent{Text: "rerun again"})
	asst2 := f.assistantCall("call-1")
	res2, err := f.reconcile(target2.ID, asst2.ID)
	require.NoError(t, err)
	require.Empty(t, res2.Voided, "a reused call id must not void the acked new incarnation: %v", res2.Voided)
	after, err := f.store.Read(f.ctx, batchRerunOwner, re.Root.RootID)
	require.NoError(t, err)
	require.Equal(t, before, after, "the new incarnation is untouched by rerun #2")
}

// Revert-check: voiding nodes without the voided-root restriction (the
// unscoped BatchRerunVoidNodesByNoticeMessageIDs) turns every deleted-notice
// row void -- the surviving root's re-pend assertions fail.
func TestBatchRerunRependSurvivingRootVoidsVoidedRoot(t *testing.T) {
	t.Parallel()
	f := newBatchRerunFx(t)

	// Root A: announce AND notices deleted -> ends void (node included).
	target := f.add(message.User, message.TextContent{Text: "rerun"})
	aAsst := f.assistantCall("call-a")
	a := f.reserve("call-a")
	f.start(a)
	aAnn := f.ack(a)
	aTaskNotice, aRootNotice := f.settle(a)
	// Root B: only its notices deleted (announce kept) -> re-pends.
	b := f.reserve("call-b")
	f.start(b)
	f.ack(b)
	bTaskNotice, bRootNotice := f.settle(b)
	// Root C: history kept -> stays done.
	c := f.reserve("call-c")
	f.start(c)
	f.ack(c)
	f.settle(c)

	res, err := f.reconcile(target.ID, aAsst.ID, aAnn.ID, aTaskNotice.ID, aRootNotice.ID, bTaskNotice.ID, bRootNotice.ID)
	require.NoError(t, err)
	require.Len(t, res.Voided, 1)
	require.Equal(t, a.Root.RootID, res.Voided[0].RootID)
	require.Equal(t, "void", f.root(a.Root.RootID).Delivery)
	require.Equal(t, "void", f.node(a.Root.RootID, a.Nodes[1].NodeID).Delivery)
	require.Equal(t, "pending", f.root(b.Root.RootID).Delivery, "a surviving root whose notice was deleted re-pends")
	require.Equal(t, "pending", f.node(b.Root.RootID, b.Nodes[1].NodeID).Delivery)
	require.Equal(t, "done", f.root(c.Root.RootID).Delivery, "an untouched root stays done")
	require.Equal(t, "done", f.node(c.Root.RootID, c.Nodes[1].NodeID).Delivery)
}

// Revert-check: dropping the target-present check deletes the tail and voids
// the old incarnation anyway.
func TestBatchRerunTargetGoneChangesNothing(t *testing.T) {
	t.Parallel()
	f := newBatchRerunFx(t)
	target := f.add(message.User, message.TextContent{Text: "rerun"})
	asst := f.assistantCall("call-1")
	old := f.reserve("call-1")
	f.start(old)
	announce := f.ack(old)
	require.NoError(t, f.messages.Delete(f.ctx, target.ID))

	_, err := f.reconcile(target.ID, asst.ID, announce.ID)
	require.ErrorIs(t, err, ErrRerunTargetGone)
	require.Equal(t, "none", f.root(old.Root.RootID).Delivery, "a refused rerun must not void anything")
	require.Equal(t, "running", f.root(old.Root.RootID).State)
}

// The legacy arm voids a root that never acked (announce_message_id NULL),
// scoped to its exact root_id. Revert-check: removing the legacy arm itself
// leaves it unvoided -- the Voided and delivery assertions fail.
func TestBatchRerunLegacyArmVoidsNeverAckedRoot(t *testing.T) {
	t.Parallel()
	f := newBatchRerunFx(t)
	target := f.add(message.User, message.TextContent{Text: "rerun"})
	asst := f.assistantCall("call-1")
	old := f.reserve("call-1")
	f.start(old)
	f.selectNotice(old, old.Root.NodeID, "root")
	require.False(t, old.Root.AnnounceMessageID.Valid, "precondition: never acked")

	res, err := f.reconcile(target.ID, asst.ID)
	require.NoError(t, err)
	require.Len(t, res.Voided, 1)
	require.Equal(t, old.Root.RootID, res.Voided[0].RootID, "the legacy arm is incarnation-scoped too")
	require.Equal(t, "completed", res.Voided[0].State)
	require.Equal(t, "void", f.root(old.Root.RootID).Delivery)
}
