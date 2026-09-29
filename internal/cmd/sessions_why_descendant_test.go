package cmd

// Regression tests for the observed production bug: `rush sessions why`
// classified the parent/root session as done merely because its per-turn
// session lock was released (stale lock) — while an implementation
// sub-agent session was still working and the outer `rush run` process was
// still alive waiting on it.
//
// The fix: a session with a live async_jobs DELEGATION row (child_session_id)
// whose owning host is not provably dead must NOT be reported done.
// explainSessionStatus consults session.AsyncJobStore.LiveDescendantJobs
// (the same cross-process derivation `sessions list` applies through
// markDelegatingLiveDescendants) and reports a distinct, non-terminal
// "delegating" verdict that names the live descendant. Stale-lock and
// crashed stay distinct from done — and from each other.
//
// Step 7 (docs/plans/2026-09-28-async-phase4-durable-core.md) replaced the
// session-lock-based descendant walk (parent_session_id + the children's
// own locks) with a durable-state walk over live async_jobs delegation
// rows (child_session_id) checked against host liveness -- these fixtures
// were rewritten from real session locks to real AsyncJobStore rows
// (still a REAL sqlite-backed store and a REAL host lock file, not a fake)
// to match.

import (
	"bytes"
	"context"
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
// OWN running/crashed/done verdict, which step 7 did not change -- only
// the cross-process DESCENDANT-work signal moved from session locks to
// async_jobs rows.
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
// hand-rolled schema, since AsyncJobStore needs async_jobs/async_hosts/
// session_notices to exist -- no full agent coordinator) plus a REAL
// AsyncJobStore wired via SetAsyncJobStoreForTest, the seam this class of
// unit test needs since it does not go through app.New/InitCoderAgent.
func newWhyDescendantTestApp(t *testing.T) (a *app.App, s session.Service, m message.Service, store *session.AsyncJobStore, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	s = session.NewService(q, conn)
	m = message.NewService(q)
	store = session.NewAsyncJobStore(conn, dataDir, 999, "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	a = &app.App{Messages: m, Sessions: s}
	a.SetAsyncJobStoreForTest(store)
	return a, s, m, store, dataDir
}

// claimDelegation claims a running 'agent' async_jobs row on parentID
// naming childID as its delegation target -- the durable fact "parentID is
// waiting on childID", live as long as the row stays 'running' on a host
// this reader judges alive (the store's own lazily-registered host is
// always alive-without-probing in-process, see HostLiveness's doc).
func claimDelegation(t *testing.T, store *session.AsyncJobStore, parentID, toolCallID, childID string) {
	t.Helper()
	_, err := store.Claim(context.Background(), session.ClaimParams{
		Owner: parentID, ToolCallID: toolCallID, Kind: session.JobKindAgent,
		Input: "delegate to " + childID, ChildSessionID: childID,
	})
	require.NoError(t, err)
}

// TestExplainSessionStatus_StaleLockLiveChildIsDelegating is the direct
// regression for the observed sequence: the parent's per-turn lock is a
// stale file naming a dead PID, its last assistant message finished with
// end_turn (the parent's own yield before the delegation), and a live
// async_jobs delegation row still names the child. Pre-fix this printed
// "status: done (stale lock)" / "Treat as done." — reporting done for work
// that was still in progress.
func TestExplainSessionStatus_StaleLockLiveChildIsDelegating(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)

	parent, err := s.Create(context.Background(), "root session waiting on sub-agent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-child", parent.ID, "implementation sub-agent")
	require.NoError(t, err)

	// PID 999999 is guaranteed not to be a live process on any platform.
	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)

	claimDelegation(t, store, parent.ID, "delegate-1", child.ID)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: delegating (stale lock)", firstLine,
		"first line must report the distinct non-terminal verdict — never done while a descendant has live work")
	require.Contains(t, out, "has live work",
		"the reason must state WHY: a descendant session has a live async_jobs row")
	require.Contains(t, out, short(session.HashID(child.ID)),
		"the reason must name the live descendant session")
	require.Contains(t, out, "NOT done",
		"the reason must make the non-terminal verdict explicit")
	require.Contains(t, out, "yield before the delegation",
		"the end_turn must be explained as the parent's own yield, not completion")
	require.NotContains(t, out, "Treat as done",
		"the stale-lock reclassification must be suppressed while descendant work is live")
	require.NotContains(t, out, "status: done")
	require.NotContains(t, out, "status: crashed",
		"a stale lock with a clean finish must not collapse into crashed either")
}

// TestExplainSessionStatus_AtRestLiveChildIsDelegating covers the other
// half of the same bug: the parent has no lock file at all (it released
// its per-turn lock when it yielded) while a live delegation row still
// names the child. Pre-fix this printed "status: at rest" / "session is
// idle".
func TestExplainSessionStatus_AtRestLiveChildIsDelegating(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)

	parent, err := s.Create(context.Background(), "at-rest session waiting on sub-agent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-atrest-child", parent.ID, "implementation sub-agent")
	require.NoError(t, err)

	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)
	claimDelegation(t, store, parent.ID, "delegate-1", child.ID)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: delegating", firstLine,
		"an at-rest parent with live descendant work must not read as at rest / idle")
	require.Contains(t, out, "no lock file present for this session")
	require.Contains(t, out, short(session.HashID(child.ID)))
	require.Contains(t, out, "NOT done")
	require.NotContains(t, out, "session is idle")
	require.NotContains(t, out, "status: at rest")
}

// TestExplainSessionStatus_ChildReleasedReturnsToDone is the companion
// control: once the delegation row reaches a terminal state, the parent is
// no longer waiting on anything and the ordinary stale-lock → done
// reclassification applies again. The promotion must be driven by a LIVE
// (state='running') row, not by the mere existence of a child session row
// or a past delegation.
func TestExplainSessionStatus_ChildReleasedReturnsToDone(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)

	parent, err := s.Create(context.Background(), "root session, sub-agent finished")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-done-child", parent.ID, "finished sub-agent")
	require.NoError(t, err)

	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)

	claimDelegation(t, store, parent.ID, "delegate-1", child.ID)
	_, err = store.Transition(context.Background(), session.TransitionParams{
		Owner: parent.ID, ToolCallID: "delegate-1", State: "completed", NoticeKind: "completed", Wake: true,
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: done (stale lock)", firstLine,
		"with no live descendant the clean-exit reclassification must apply again")
	require.Contains(t, out, "Treat as done")
	require.NotContains(t, out, "delegating")
}

// TestExplainSessionStatus_GrandchildLiveIsDelegating proves transitivity:
// the root's own async_jobs row delegates to the child, and the CHILD's own
// async_jobs row in turn delegates to the grandchild — both rows 'running'
// (a delegation stays running for exactly as long as the delegate is still
// working, at every level), so the root must be reported non-terminal even
// though the only NEW work in this turn happened two levels down.
func TestExplainSessionStatus_GrandchildLiveIsDelegating(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)

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

	claimDelegation(t, store, root.ID, "delegate-root-child", child.ID)
	claimDelegation(t, store, child.ID, "delegate-child-grand", grandchild.ID)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, root.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: delegating (stale lock)", firstLine,
		"a live GRANDCHILD delegation must hold the root non-terminal — the walk is transitive")
	require.Contains(t, out, short(session.HashID(child.ID)),
		"the reason names the immediate child the root's own delegation row points at")
	require.NotContains(t, out, "Treat as done")
}

// TestExplainSessionStatus_CrashedStaysCrashedWithLiveChild proves the
// statuses stay distinct: a parent whose holder is genuinely dead and
// whose last turn did NOT finish cleanly remains "crashed" — a live
// delegation must neither promote it to done nor be used to explain it
// away as crashed. The child, asked about ITS OWN status (via its own real
// session lock, unrelated to the delegation-row mechanism), reports
// "running": two distinct truthful verdicts for two distinct sessions.
func TestExplainSessionStatus_CrashedStaysCrashedWithLiveChild(t *testing.T) {
	t.Parallel()
	a, s, m, store, dataDir := newWhyDescendantTestApp(t)

	parent, err := s.Create(context.Background(), "crashed parent with live child")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-crashed-child", parent.ID, "still-working sub-agent")
	require.NoError(t, err)

	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	// Canceled, not end_turn — no clean finish, so this is a genuine crash.
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonCanceled)

	claimDelegation(t, store, parent.ID, "delegate-1", child.ID)

	// The child's OWN status still comes from its OWN session lock -- that
	// mechanism is unrelated to (and unchanged by) the delegation-row
	// liveness walk above.
	childLock, err := session.TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, childLock.Release()) }()

	var parentBuf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &parentBuf))
	parentOut := parentBuf.String()
	firstLine := strings.SplitN(parentOut, "\n", 2)[0]
	require.Equal(t, "status: crashed", firstLine,
		"a parent with a dead holder and no clean finish stays crashed — distinct from done and from the child's state")
	require.Contains(t, parentOut, "died mid-turn")
	require.NotContains(t, parentOut, "delegating")
	require.NotContains(t, parentOut, "status: done")

	var childBuf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, child.ID, &childBuf))
	childOut := childBuf.String()
	childFirstLine := strings.SplitN(childOut, "\n", 2)[0]
	require.Equal(t, "status: running", childFirstLine,
		"the descendant holding the live lock must itself report running")
}

// TestExplainSessionStatus_AsyncJobsAndDebtSection covers the plain-language
// "Async jobs:" section (doc sec.5 step 7): which jobs are running, and
// whether a reaction debt is pending -- a completed job's result already
// sitting in delivery='done' with no model turn having reacted to it yet.
func TestExplainSessionStatus_AsyncJobsAndDebtSection(t *testing.T) {
	t.Parallel()
	a, s, _, store, dataDir := newWhyDescendantTestApp(t)
	ctx := context.Background()

	sess, err := s.Create(ctx, "session with jobs and debt")
	require.NoError(t, err)

	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: sess.ID, ToolCallID: "running-1", Kind: session.JobKindCommand, Input: "sleep 100", ToolName: "bash",
	})
	require.NoError(t, err)

	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: sess.ID, ToolCallID: "done-1", Kind: session.JobKindCommand, Input: "echo hi", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, sess.ID, "done-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "done-1", State: "completed", NoticeKind: "completed", Wake: true, Delivery: "done",
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Contains(t, out, "Async jobs:")
	require.Contains(t, out, "2 total, 1 running")
	require.Contains(t, out, "running: tool call running-1")
	require.Contains(t, out, "reaction debt: pending",
		"done-1's result is delivered (delivery='done') but no model turn has reacted to it yet")

	// C12: a completed job whose notice is still 'pending' (nothing has pulled
	// it into history yet) is ALSO reaction debt (DUR-4) -- it used to be
	// reported as "none" because only the delivered half was consulted.
	// Revert-check: read VisibleReactionDebtExists only (the pre-fix code) and
	// this case prints "reaction debt: none".
	pendingSess, err := s.Create(ctx, "session with a not-yet-delivered completion")
	require.NoError(t, err)
	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: pendingSess.ID, ToolCallID: "pending-1", Kind: session.JobKindCommand, Input: "echo pending", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, pendingSess.ID, "pending-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: pendingSess.ID, ToolCallID: "pending-1", State: "completed", NoticeKind: "completed", Wake: true,
	})
	require.NoError(t, err)

	var pendingBuf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, pendingSess.ID, &pendingBuf))
	pendingOut := pendingBuf.String()
	require.Contains(t, pendingOut, "reaction debt: pending")
	require.Contains(t, pendingOut, "not been delivered into history yet")
	require.NotContains(t, pendingOut, "reaction debt: none",
		"a pending (undelivered) completion is reaction debt, not none")
}

// TestExplainSessionStatus_AsyncJobsSection_NoDebtOnceReacted proves the
// debt line flips to "none" once MarkAsyncJobsReactedForOwner-equivalent
// state is reached (Reacted=true at transition time).
func TestExplainSessionStatus_AsyncJobsSection_NoDebtOnceReacted(t *testing.T) {
	t.Parallel()
	a, s, _, store, dataDir := newWhyDescendantTestApp(t)
	ctx := context.Background()

	sess, err := s.Create(ctx, "session with reacted job")
	require.NoError(t, err)

	_, err = store.Claim(ctx, session.ClaimParams{
		Owner: sess.ID, ToolCallID: "done-1", Kind: session.JobKindCommand, Input: "echo hi", ToolName: "bash",
	})
	require.NoError(t, err)
	require.NoError(t, store.MarkAnnounced(ctx, sess.ID, "done-1"))
	_, err = store.Transition(ctx, session.TransitionParams{
		Owner: sess.ID, ToolCallID: "done-1", State: "completed", NoticeKind: "completed",
		Wake: true, Delivery: "done", Reacted: true,
	})
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &buf))
	out := buf.String()

	require.Contains(t, out, "reaction debt: none")
}
