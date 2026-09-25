package cmd

// Regression tests for the observed production bug: `rush sessions why`
// classified the parent/root session as done merely because its per-turn
// session lock was released (stale lock) — while an implementation
// sub-agent session held a LIVE lock and the outer `rush run` process was
// still alive waiting on that sub-agent. Done was reported for work that
// was still in progress.
//
// The fix: a session whose DESCENDANT sessions are still working must NOT
// be reported done. explainSessionStatus consults session.LiveDescendants
// (the same cross-process derivation `sessions list` applies through
// markDelegatingLiveDescendants) and reports a distinct, non-terminal
// "delegating" verdict that names the live descendant. Stale-lock and
// crashed stay distinct from done — and from each other.
//
// Fixture note: the child's lock is a REAL exclusive lock acquired
// in-process via session.TryAcquireSessionLock (released through defer),
// so the lock file, its PID sidecar and its heartbeat are all genuine —
// not a forged file.

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
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// writeLockFileAt writes a session lock file holding the given PID under
// <dataDir>/locks/ and returns its path. Unlike writeLockFile
// (sessions_why_test.go), which allocates a fresh temp dir per call, this
// variant lets one test drive the parent's stale lock and the child's real
// lock in the SAME locks directory.
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

// addEndTurnAssistant records a finished assistant message on sessionID
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

// TestExplainSessionStatus_StaleLockLiveChildIsDelegating is the direct
// regression for the observed sequence: the parent's per-turn lock is a
// stale file naming a dead PID, its last assistant message finished with
// end_turn (the parent's own yield before the delegation), and a child
// session holds a REAL live lock. Pre-fix this printed
// "status: done (stale lock)" / "Treat as done." — reporting done for work
// that was still in progress.
func TestExplainSessionStatus_StaleLockLiveChildIsDelegating(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(context.Background(), "root session waiting on sub-agent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-child", parent.ID, "implementation sub-agent")
	require.NoError(t, err)

	dataDir := t.TempDir()
	// PID 999999 is guaranteed not to be a live process on any platform.
	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)

	childLock, err := session.TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, childLock.Release()) }()

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: delegating (stale lock)", firstLine,
		"first line must report the distinct non-terminal verdict — never done while a descendant holds a live lock")
	require.Contains(t, out, "holds a live lock",
		"the reason must state WHY: a descendant session holds a live lock")
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
// its per-turn lock when it yielded) while the child still holds a live
// one. Pre-fix this printed "status: at rest" / "session is idle".
func TestExplainSessionStatus_AtRestLiveChildIsDelegating(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(context.Background(), "at-rest session waiting on sub-agent")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-atrest-child", parent.ID, "implementation sub-agent")
	require.NoError(t, err)

	dataDir := t.TempDir()
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)

	childLock, err := session.TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, childLock.Release()) }()

	a := &app.App{Messages: m, Sessions: s}
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
// control: once the child's lock is gone (released longer ago than
// LockStaleDuration), the parent is no longer waiting on anything and the
// ordinary stale-lock → done reclassification applies again. The promotion
// must be driven by live descendant work, not by the mere existence of a
// child session row.
func TestExplainSessionStatus_ChildReleasedReturnsToDone(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(context.Background(), "root session, sub-agent finished")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-done-child", parent.ID, "finished sub-agent")
	require.NoError(t, err)

	dataDir := t.TempDir()
	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonEndTurn)

	childLock, err := session.TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	require.NoError(t, childLock.Release())

	// Release truncates the lock file rather than unlinking it, which
	// leaves a fresh mtime behind; age it past the post-release window so
	// it reads as genuinely gone.
	releasedAgo := time.Now().Add(-(session.LockStaleDuration + 5*time.Second))
	require.NoError(t, os.Chtimes(session.SessionLockPath(dataDir, child.ID), releasedAgo, releasedAgo))

	a := &app.App{Messages: m, Sessions: s}
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
// an intermediate child may be entirely at rest while a GRANDCHILD still
// holds a live lock, and the root must still be reported non-terminal.
func TestExplainSessionStatus_GrandchildLiveIsDelegating(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	root, err := s.Create(context.Background(), "root of a three-level tree")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-mid", root.ID, "intermediate sub-agent")
	require.NoError(t, err)
	grandchild, err := s.CreateTaskSession(context.Background(), "why-desc-grand", child.ID, "sub-sub-agent")
	require.NoError(t, err)

	dataDir := t.TempDir()
	// The root released its per-turn lock when it yielded, leaving only a
	// stale file naming a dead PID behind — the observed production shape.
	rootLock := writeLockFileAt(t, dataDir, root.ID, 999999)
	backDateLock(t, rootLock)
	addFinishedAssistant(t, m, root.ID, message.FinishReasonEndTurn)

	// Only the grandchild holds a live lock; the intermediate child is at rest.
	grandchildLock, err := session.TryAcquireSessionLock(dataDir, grandchild.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, grandchildLock.Release()) }()

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, root.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: delegating (stale lock)", firstLine,
		"a live GRANDCHILD must hold the root non-terminal — the walk is transitive")
	require.Contains(t, out, short(session.HashID(grandchild.ID)),
		"the reason must name the live descendant that was actually found")
	require.NotContains(t, out, "Treat as done")
}

// TestExplainSessionStatus_CrashedStaysCrashedWithLiveChild proves the
// statuses stay distinct: a parent whose holder is genuinely dead and
// whose last turn did NOT finish cleanly remains "crashed" — the live
// child must neither promote it to done nor be used to explain it away as
// crashed. The child, asked about itself, reports "running": two distinct
// truthful verdicts for two distinct sessions.
func TestExplainSessionStatus_CrashedStaysCrashedWithLiveChild(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	parent, err := s.Create(context.Background(), "crashed parent with live child")
	require.NoError(t, err)
	child, err := s.CreateTaskSession(context.Background(), "why-desc-crashed-child", parent.ID, "still-working sub-agent")
	require.NoError(t, err)

	dataDir := t.TempDir()
	parentLock := writeLockFileAt(t, dataDir, parent.ID, 999999)
	backDateLock(t, parentLock)
	// Canceled, not end_turn — no clean finish, so this is a genuine crash.
	addFinishedAssistant(t, m, parent.ID, message.FinishReasonCanceled)

	childLock, err := session.TryAcquireSessionLock(dataDir, child.ID)
	require.NoError(t, err)
	defer func() { require.NoError(t, childLock.Release()) }()

	a := &app.App{Messages: m, Sessions: s}

	var parentBuf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, parent.ID, &parentBuf))
	parentOut := parentBuf.String()
	firstLine := strings.SplitN(parentOut, "\n", 2)[0]
	require.Equal(t, "status: crashed", firstLine,
		"a parent with a dead holder and no clean finish stays crashed — distinct from done and from the child's state")
	require.Contains(t, parentOut, "died mid-turn")
	require.NotContains(t, parentOut, "delegating")
	require.NotContains(t, parentOut, "status: done")

	// The child, asked about itself, is running: the same underlying state
	// yields two distinct, per-session truthful verdicts.
	var childBuf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, dataDir, child.ID, &childBuf))
	childOut := childBuf.String()
	childFirstLine := strings.SplitN(childOut, "\n", 2)[0]
	require.Equal(t, "status: running", childFirstLine,
		"the descendant holding the live lock must itself report running")
}
