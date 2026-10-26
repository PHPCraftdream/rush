package cmd

// Regression tests for the observed production bug: `rush sessions why`
// classified the parent/root session as done merely because its per-turn
// session lock was released (stale lock) — while an implementation
// sub-agent session was still working and the outer `rush run` process was
// still alive waiting on it.
//
// The fix: a session with LIVE batch delegation work (a nonterminal batch
// root whose task child carries the child session link) on a host that is
// not provably dead must NOT be reported done. explainSessionStatus routes
// to explainWhyBatch, which reads the batch facts through the same
// cross-process derivation `sessions list` applies, and reports the
// conservative "batch live" verdict that names the held root. Stale-lock
// and crashed stay distinct from done — and from each other.

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// writeLockFileAt writes a session lock file holding the given PID under
// <dataDir>/locks/ and returns its path. Still used for the PARENT/CHILD's
// OWN running/crashed/done verdict -- only the cross-process
// DESCENDANT-work signal comes from the batch roots now.
func writeLockFileAt(t *testing.T, dataDir, sessionID string, pid int) string {
	t.Helper()
	locksDir := filepath.Join(dataDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	lockPath := filepath.Join(locksDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(strconv.Itoa(pid)+"\n"), 0o644))
	return lockPath
}

// backDateLock ages a lock file past LockStaleDuration so it reads as
// abandoned rather than merely quiet.
func backDateLock(t *testing.T, path string) {
	t.Helper()
	stale := time.Now().Add(-(session.LockStaleDuration + 5*time.Second))
	require.NoError(t, os.Chtimes(path, stale, stale))
}

// addFinishedAssistant records a finished assistant message on sessionID
// with the given finish reason — the message-store half of the
// "clean finish" reclassification rule.
func addFinishedAssistant(t *testing.T, m message.Service, sessionID string, reason message.FinishReason) {
	t.Helper()
	assistant, err := m.Create(context.Background(), sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "delegated the work"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(reason, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))
}

// newWhyDescendantTestApp builds a lightweight *app.App (session + message
// services over a REAL, fully migrated sqlite DB -- db.Connect, not a
// hand-rolled schema, since the batch readers need batch_nodes and
// session_notices to exist -- no full agent coordinator).
func newWhyDescendantTestApp(t *testing.T) (a *app.App, s session.Service, m message.Service, store *session.SessionNoticeStore, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	s = session.NewService(q, conn)
	m = message.NewService(q)
	a = &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetDataDirForTest(dataDir)
	return a, s, m, session.NewSessionNoticeStore(conn), dataDir
}

// claimDelegation seeds the "parentID is waiting on childID" fact: a live
// batch root whose task child carries the ChildSessionID link (set directly
// in the reservation's child Values -- db.ReserveBatchNodeParams has the
// field). The root stays live as long as it stays nonterminal on a host
// this reader judges alive (the fixture's own registered host is always
// alive-without-probing in-process).
func claimDelegation(t *testing.T, a *app.App, parentID, toolCallID, childID string) {
	t.Helper()
	host, bs := batchSeedHost(t, a)
	_, err := bs.Reserve(context.Background(), session.BatchReservation{
		Owner: parentID, Host: host, RunnerID: "claim-delegation-runner",
		ToolCallID: toolCallID, InputHash: "hash-" + toolCallID, Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{
				Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "pending", ControlState: "none",
				ChildSessionID: sql.NullString{String: childID, Valid: true},
			}},
		}},
	})
	require.NoError(t, err)
}

// TestExplainSessionStatus_StaleLockLiveChildIsDelegating is the direct
// regression for the observed sequence: the parent's per-turn lock is a
// stale file naming a dead PID, its last assistant message finished with
// end_turn (the parent's own yield before the delegation), and a live batch
// delegation still names the child. Pre-fix this printed "status: done
// (stale lock)" / "Treat as done." — reporting done for work still in
// progress.
func TestExplainSessionStatus_StaleLockLiveChildIsDelegating(t *testing.T) {
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)

	parent, err := s.Create(context.Background(), "root session waiting on sub-agent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-child", parent.ID, "implementation sub-agent")
	require.NoError(t, err)

	// PID 999999 is guaranteed not to be a live process on any platform.
	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)

	claimDelegation(t, a, parent.ID, "delegate-1", child.ID)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: batch live (stale lock)", firstLine,
		"first line must report the conservative live verdict — never done while a descendant has live work")
	require.Contains(t, out, "reason: batch root ",
		"the reason must state WHY: a descendant delegation root is held")
	require.Contains(t, out, "dead PID 999999",
		"the recorded dead PID rides along as an annotation (D1)")
	require.NotContains(t, out, "status: done")
	require.NotContains(t, out, "status: crashed",
		"live work outranks the dead PID (D1)")
}

// TestExplainSessionStatus_AtRestLiveChildIsDelegating covers the other
// half of the same bug: the parent has no lock file at all (it released
// its per-turn lock when it yielded) while a live delegation still
// names the child. Pre-fix this printed "status: at rest" / "session is
// idle".
func TestExplainSessionStatus_AtRestLiveChildIsDelegating(t *testing.T) {
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)

	parent, err := s.Create(context.Background(), "at-rest session waiting on sub-agent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-atrest-child", parent.ID, "implementation sub-agent")
	require.NoError(t, err)

	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)
	claimDelegation(t, a, parent.ID, "delegate-1", child.ID)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: batch live", firstLine,
		"an at-rest parent with live descendant work must not read as at rest / idle")
	require.Contains(t, out, "delegate-1", "the held root's tool call is named")
	require.NotContains(t, out, "session is idle")
	require.NotContains(t, out, "status: at rest")
}

// TestExplainSessionStatus_ChildReleasedReturnsToDone is the companion
// control: once the delegation root reaches a terminal state, the parent is
// no longer waiting on anything and the ordinary stale-lock → crashed
// classification applies again. The promotion must be driven by a LIVE
// (nonterminal) root, not by the mere existence of a child session row
// or a past delegation.
func TestExplainSessionStatus_ChildReleasedReturnsToDone(t *testing.T) {
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)

	parent, err := s.Create(context.Background(), "root session, sub-agent finished")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-done-child", parent.ID, "finished sub-agent")
	require.NoError(t, err)

	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)

	claimDelegation(t, a, parent.ID, "delegate-1", child.ID)
	settleBatchRoot(t, a, parent.ID, "delegate-1")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	// D3/D4: dead recorded PID, no live work, and end_turn is not an end
	// signal -> crashed.
	require.Equal(t, "status: crashed", firstLine,
		"a dead PID without live work is crashed even after end_turn and a completed delegation")
	require.Contains(t, out, "no longer alive")
	require.NotContains(t, out, "batch live")
}

// TestExplainSessionStatus_GrandchildLiveIsDelegating proves transitivity:
// the root's own delegation names the child, and the CHILD's own delegation
// in turn names the grandchild — both roots nonterminal, so the root must
// be reported non-terminal even though the only NEW work in this turn
// happened two levels down.
func TestExplainSessionStatus_GrandchildLiveIsDelegating(t *testing.T) {
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)

	root, err := s.Create(context.Background(), "root of a three-level tree")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-mid", root.ID, "intermediate sub-agent")
	require.NoError(t, err)
	grandchild, err := s.CreateTaskSession(context.Background(), "why-desc-grand", child.ID, "sub-sub-agent")
	require.NoError(t, err)

	// The root released its per-turn lock when it yielded, leaving only a
	// stale file naming a dead PID behind — the observed production shape.
	rootLock := writeLockFileAt(t, dataDir, root.ID, 999999)
	backDateLock(t, rootLock)
	addFinishedAssistant(t, m, root.ID, message.FinishReasonEndTurn)

	claimDelegation(t, a, root.ID, "delegate-root-child", child.ID)
	claimDelegation(t, a, child.ID, "delegate-child-grand", grandchild.ID)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, root.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: batch live (stale lock)", firstLine,
		"a live GRANDCHILD delegation must hold the root non-terminal — the walk is transitive")
	require.Contains(t, out, "reason: batch root ",
		"the reason names the held root (transitive walk)")
	require.NotContains(t, out, "Treat as done")
}

// TestExplainSessionStatus_DeadHolderWithLiveChildIsDelegating: a
// parent whose holder is genuinely dead but which has a live delegation is
// "batch live (stale lock)" -- live work outranks a dead PID, which is
// demoted to an annotation. The child, asked about ITS OWN status (via its
// own real session lock), reports "running": two distinct truthful verdicts
// for two distinct sessions.
func TestExplainSessionStatus_DeadHolderWithLiveChildIsDelegating(t *testing.T) {
	a, s, m, _, dataDir := newWhyDescendantTestApp(t)

	parent, err := s.Create(context.Background(), "crashed parent with live child")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-crashed-child", parent.ID, "still-working sub-agent")
	require.NoError(t, err)

	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	// Canceled, not end_turn — no clean finish, so this is a genuine crash.
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonCanceled)

	claimDelegation(t, a, parent.ID, "delegate-1", child.ID)

	// The child's OWN status still comes from its OWN session lock -- that
	// mechanism is unrelated to (and unchanged by) the batch-root liveness
	// walk above.
	childLock, err := session.TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, childLock.Release()) }()

	var parentBuf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &parentBuf))
	parentOut := parentBuf.String()
	firstLine := strings.SplitN(parentOut, "\n", 2)[0]
	// D1/D2: live work (the delegation) outranks the dead recorded PID,
	// which becomes the stale-lock annotation.
	require.Equal(t, "status: batch live (stale lock)", firstLine,
		"a live delegation outranks a dead PID -- distinct from done and from the child's state")
	require.NotContains(t, parentOut, "status: done")

	var childBuf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, child.ID, &childBuf))
	childOut := childBuf.String()
	childFirstLine := strings.SplitN(childOut, "\n", 2)[0]
	require.Equal(t, "status: running", childFirstLine,
		"the descendant holding the live lock must itself report running")
}

// TestExplainSessionStatus_BatchRootDebtLine covers the debt fact of the
// `sessions why` batch section: a HELD (pending) root whose task child
// carries delivered-but-unreacted completion debt renders a nonzero per-root
// debt, and the debt disappears once the notice stops being debt (wake=0).
//
// Snapshot finding (batch_history_snapshot.go): the per-root debt is the
// count of the root's level-subtree rows satisfying batchHistoryDebtRow
// (notice_selected=1, wake=1, reacted=0, delivery != 'void');
// BatchLiveNodesForOwners/BatchLevelNodes read every row of a nonterminal
// root regardless of the row's own state, so a pending root with a debt
// carrying task renders Debt=1 — while a TERMINAL root is excluded from the
// live read entirely and can never render.
//
// Revert-check: dropping the debt read from reduceBatchSessionFacts (or the
// debt column in renderBatchRootsForWhy) fails the first assertion.
func TestExplainSessionStatus_BatchRootDebtLine(t *testing.T) {
	a, s, _, _, dataDir := newWhyDescendantTestApp(t)
	ctx := context.Background()

	sess, err := s.Create(ctx, "session with held batch debt")
	require.NoError(t, err)

	host, bs := batchSeedHost(t, a)
	res, err := bs.Reserve(ctx, session.BatchReservation{
		Owner: sess.ID, Host: host, RunnerID: "why-debt-runner",
		ToolCallID: "done-1", InputHash: "hash-done-1", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{
			Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true},
			State: "pending", ControlState: "none",
		}, Children: []session.BatchReservationNode{
			{Values: db.ReserveBatchNodeParams{Kind: "task", TaskKind: sql.NullString{String: "command", Valid: true}, State: "pending", ControlState: "none"}},
		}},
	})
	require.NoError(t, err)
	rootID := res.Snapshot.Root.RootID
	var nodeID string
	require.NoError(t, a.DB().QueryRowContext(ctx,
		`SELECT node_id FROM batch_nodes WHERE root_id = ? AND kind = 'task'`, rootID).Scan(&nodeID))
	// Seed delivered-but-unreacted completion debt with the same raw UPDATEs
	// the debt fixtures use (announced root with announce_message_id;
	// selected task notice, delivery='done', wake=1). The root stays PENDING
	// (held) so the live read still shows it.
	resAnnounce, err := a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET announced = 1, announce_message_id = 'why-debt-msg'
		 WHERE node_id = ? AND root_id = ?`, rootID, rootID)
	require.NoError(t, err)
	rows, _ := resAnnounce.RowsAffected()
	require.EqualValues(t, 1, rows, "the root announce UPDATE must hit exactly the root row")
	resDebt, err := a.DB().ExecContext(ctx,
		`UPDATE batch_nodes SET notice_selected = 1, notice_kind = 'task', notice_state = 'completed',
		 notice_cause = '', notice_sequence = 1, notice_ordinal = 0, delivery = 'done', wake = 1
		 WHERE node_id = ? AND parent_id = ?`, nodeID, rootID)
	require.NoError(t, err)
	rows, _ = resDebt.RowsAffected()
	require.EqualValues(t, 1, rows, "the debt UPDATE must hit exactly the task row")

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &buf))
	out := buf.String()
	require.Contains(t, out, "done-1", "the held root is named under Batch roots")
	require.Regexp(t, `debt 1`, out,
		"done-1's result is delivered (delivery='done') but no model turn has reacted to it yet")

	// Once the notice stops being debt (wake=0) the debt column drops to 0.
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	resWake, err := conn.ExecContext(ctx, `UPDATE batch_nodes SET wake = 0 WHERE node_id = ?`, nodeID)
	require.NoError(t, err)
	rows, _ = resWake.RowsAffected()
	require.EqualValues(t, 1, rows, "the wake flip must hit exactly the debt row")
	require.NoError(t, db.ReleaseConn(conn))

	buf.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &buf))
	require.Regexp(t, `debt 0`, buf.String(),
		"a reacted (wake=0) notice is no longer debt")
}
