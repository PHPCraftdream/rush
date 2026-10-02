package cmd

import (
	"bytes"
	"context"
	"io"
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

// explainWhy points a hand-built App's lock reader at dataDir (the seam
// explainSessionStatus stopped setting itself out of production code) and
// renders the verdict into out.
func explainWhy(a *app.App, dataDir, sessionID string, out io.Writer) error {
	a.SetDataDirForTest(dataDir)
	return explainSessionStatus(context.Background(), a, dataDir, sessionID, out)
}

// writeLockFile creates a session lock file under tmpDir/locks/ holding
// the given PID (second line = optional timeout seconds), and returns the
// tmpDir so it can be passed to explainSessionStatus as its dataDir
// parameter — explainSessionStatus resolves the lock at
// <dataDir>/locks/session-<id>.lock (task #233 fix; previously this helper
// nested an extra ".rush" level to match the pre-fix <cwd>/.rush/locks
// layout).
func writeLockFile(t *testing.T, sessionID string, pid int) string {
	t.Helper()
	tmpDir := t.TempDir()
	locksDir := filepath.Join(tmpDir, "locks")
	require.NoError(t, os.MkdirAll(locksDir, 0o755))
	lockPath := filepath.Join(locksDir, "session-"+sanitiseSessionIDForFilename(sessionID)+".lock")
	require.NoError(t, os.WriteFile(lockPath, []byte(strconv.Itoa(pid)+"\n"), 0o644))
	return tmpDir
}

// TestExplainSessionStatus_Done_EndTurn: no lock file → "at rest", but the
// last assistant message finished with end_turn, so the output should say
// the session is idle and mention end_turn.
func TestExplainSessionStatus_AtRest_CleanFinish(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "clean idle")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "do it"}},
	})
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "done"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	// t.TempDir() with no locks dir created → no lock file → "at rest".
	require.NoError(t, explainWhy(a, t.TempDir(), sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: at rest")
	require.Contains(t, out, "end_turn")
}

// TestExplainSessionStatus_Crashed_NoCleanFinish: lock file exists, holder PID
// is dead, and the last assistant message did NOT finish cleanly → verdict is
// "crashed" and the reason mentions dying mid-turn.
func TestExplainSessionStatus_Crashed_NoCleanFinish(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "mid-turn crash")
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "partial"}},
	})
	require.NoError(t, err)
	// Canceled, not end_turn — no clean finish.
	assistant.AddFinish(message.FinishReasonCanceled, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	// PID 999999 is guaranteed not to be a live process on any platform.
	cwd := writeLockFile(t, sess.ID, 999999)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: crashed")
	// CHANGED STRING (R-ACT-2): the classifier's dead-PID phrasing replaced
	// the old "died mid-turn" prose.
	require.Contains(t, out, "no longer alive")
	require.Contains(t, out, "canceled")
}

// TestExplainSessionStatus_StaleLockCleanFinish: lock file exists, holder
// PID is dead, AND the last assistant message finished with end_turn.
// CHANGED VERDICT (R-ACT-2, D3/D4): a recorded dead PID means the holder
// never reached release; the end_turn finish of a previous turn is not an
// end signal (only ended_reason is). The verdict is therefore "crashed" --
// the old "done (stale lock)" reclassification by finish reason is gone.
func TestExplainSessionStatus_StaleLockCleanFinish(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "stale lock clean exit")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "run"}},
	})
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "finished ok"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	cwd := writeLockFile(t, sess.ID, 999999)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, cwd, sess.ID, &buf))

	out := buf.String()
	firstLine := strings.SplitN(out, "\n", 2)[0]
	require.Equal(t, "status: crashed", firstLine,
		"a dead recorded PID with no live work is crashed; an end_turn finish is not an end signal")
	require.NotContains(t, out, "done (stale lock)")
	require.Contains(t, out, "no longer alive")
}

// TestExplainSessionStatus_AtRest_NoAssistantMessage: no lock file and no
// assistant message at all → "at rest" with the "no assistant message" note.
func TestExplainSessionStatus_AtRest_NoAssistantMessage(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "empty")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, t.TempDir(), sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: at rest")
	// CHANGED STRING (R-ACT-2): the finish context is the "Last assistant
	// message" section now; the verdict line no longer carries the note.
	require.Contains(t, out, "(none)")
}

// TestExplainSessionStatus_Running: lock file exists and holder PID is alive
// (we use our own PID) → verdict is "running" and the heartbeat age is shown.
func TestExplainSessionStatus_Running(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "live run")
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "working"}},
	})
	require.NoError(t, err)
	// Tool use finish — turn still in progress from the user's perspective,
	// but the message has a finish part we can report.
	assistant.AddFinish(message.FinishReasonToolUse, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	cwd := writeLockFile(t, sess.ID, os.Getpid())

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: running")
	// CHANGED STRING (R-ACT-2): the reason names the holder PID; the
	// heartbeat age is gone (D10 -- no mtime in the enum).
	require.Contains(t, out, "held by PID")
	require.Contains(t, out, "tool_use")
}

// TestExplainSessionStatus_Running_PIDUnreadableFreshHeartbeat reproduces
// the Windows scenario: tryLockFile's mandatory LockFileEx lock means the
// PID can't be read from another process while the holder is alive, so
// ReadLockPID returns 0 even though the session is genuinely running. A
// fresh heartbeat (recent mtime) must still classify this as "running", not
// "crashed" — see the Windows note on session.readLockFile.
func TestExplainSessionStatus_Running_PIDUnreadableFreshHeartbeat(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "live run, unreadable pid")
	require.NoError(t, err)

	// pid=0 simulates a lock file whose PID line couldn't be read.
	cwd := writeLockFile(t, sess.ID, 0)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, cwd, sess.ID, &buf))

	out := buf.String()
	// CHANGED VERDICT (R-ACT-2, D10): a readable record with no PID is a
	// RELEASE whatever the mtime -- pid=0 no longer falls back to heartbeat
	// freshness, so this shape now reads at rest, not running.
	require.Contains(t, out, "status: at rest")
	require.NotContains(t, out, "status: crashed")
}

// TestExplainSessionStatus_PIDlessRecordIsReleased: a readable record with
// no PID is a release whatever the mtime (D10; the #258 "never invent a
// fictional PID 0 holder" property survives trivially -- no mtime, no
// heartbeat wording, no PID named).
//
// CHANGED VERDICT (R-ACT-2): the pre-rework shape reported "crashed" via
// the stale-heartbeat fallback; a PID-less record now reads released, and
// with no live work and no ended_reason the verdict is at rest.
func TestExplainSessionStatus_PIDlessRecordIsReleased(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "dead run, unreadable pid")
	require.NoError(t, err)

	cwd := writeLockFile(t, sess.ID, 0)
	lockPath := filepath.Join(cwd, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	old := time.Now().Add(-30 * time.Second)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: at rest")
	require.NotContains(t, out, "PID 0",
		"no fictional PID 0 holder is ever named (task #258's property)")
	require.NotContains(t, out, "heartbeat",
		"the mtime/heartbeat wording is gone entirely (D10)")
}

// TestExplainSessionStatus_ErrorFinishSurfacesErrorText: when the last
// assistant message finished with FinishReasonError and stored error text,
// the output must include that error text so the operator sees the cause.
func TestExplainSessionStatus_ErrorFinishSurfacesErrorText(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "errored")
	require.NoError(t, err)

	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "oops"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonError, "upstream 502: bad gateway", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	cwd := writeLockFile(t, sess.ID, 999999)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, cwd, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: crashed")
	require.Contains(t, out, "no longer alive")
	require.Contains(t, out, "error")
	require.Contains(t, out, "upstream 502: bad gateway")
}

// The MaxPidFallbackAge PID-reuse bound (tasks #250/#256/#257) is GONE under
// the R-ACT-2 lock enum (D10): a readable record naming an alive PID is
// held, whatever the lock's age, and there is no second, age-based
// formulation. CHANGED VERDICT: an aged lock whose recorded PID is alive
// (the OS-reuse scenario #250 guarded) now reads running, not crashed -- the
// reuse-pinning risk is accepted by the architect's decision (release wipes
// the PID, so a recorded PID means "never reached release"). The dead-PID
// phrasing keeps its #257 property: it never claims a factually-alive PID
// "is not alive", because that branch only fires on IsProcessAlive=false.
//
// Revert-check: re-adding an mtime-based bound anywhere in the lock reader
// breaks TestInspectSessionLockFact (mtime is never consulted) and this
// test's running expectation.
func TestExplainSessionStatus_AgedLockWithLivePIDIsRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a real child process; skipped in -short")
	}

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "why-status-pid-reuse")
	require.NoError(t, err)

	dataDir := t.TempDir()
	holder := spawnKillTestLockHolder(t, dataDir, sess.ID, false)
	defer holder.stop(t)
	require.True(t, session.IsProcessAlive(holder.pid), "helper process must still be alive for this test to be meaningful")

	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	staleTime := time.Now().Add(-(session.MaxPidFallbackAge + 5*time.Second))
	require.NoError(t, os.Chtimes(lockPath, staleTime, staleTime),
		"back-dating mtime: the enum must not depend on it")

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: running",
		"an alive recorded PID is held, whatever the lock's age (D10)")
	require.Contains(t, out, "held by PID")
	require.NotContains(t, out, "is not alive",
		"the recorded PID is factually alive in this scenario -- the dead phrasing must never fire here")
}

// A genuinely dead recorded PID (any age) is crashed, and the reason uses
// the plain dead-PID phrasing (the #257 property survives the rework: the
// classifier only says "no longer alive" after IsProcessAlive said so).
func TestExplainSessionStatus_DeadPIDIsCrashed(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "why-status-pid-bound-genuinely-dead")
	require.NoError(t, err)

	// PID 999999 is guaranteed not to be a live process on any platform.
	dataDir := writeLockFile(t, sess.ID, 999999)

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: crashed")
	require.Contains(t, out, "no longer alive",
		"the recorded PID is genuinely dead here -- the reason must say so plainly")
	require.NotContains(t, out, "OS PID reuse",
		"the PID-reuse wording no longer exists at all (D10)")
}

// A lock whose state cannot be read at all is fail-open: the classifier
// answers in turn ("assuming live") -- ASYNC-02's convention. CHANGED OUTPUT
// (R-ACT-2): the old wording was "status: unknown (could not verify)"; the
// verdict vocabulary now has no unknown Kind, so the same distinction
// ("could not check" is not "verifiably absent") is carried by the
// fail-open in-turn verdict plus an explicit "could not be read" reason.
//
// Revert-check: making an unreadable lock classify as released/idle flips
// the first assertion to "status: at rest".
func TestExplainSessionStatus_StatFailureIsFailOpenInTurn(t *testing.T) {
	t.Parallel()

	conn, q := newTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)

	sess, err := s.Create(context.Background(), "stat-fail")
	require.NoError(t, err)

	_, err = m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	// Forge a real non-ENOENT stat failure with a NUL byte in dataDir: Go's
	// os package rejects NUL bytes in a path before any syscall, on every
	// platform, so this can never be misclassified as "not found".
	dataDir := t.TempDir() + string([]byte{0}) + "bad"
	lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
	_, err = os.Stat(lockPath)
	require.Error(t, err, "stat of a NUL-containing path must fail")
	require.False(t, os.IsNotExist(err),
		"this failure must NOT be ENOENT -- we need a different stat error class for this test")

	a := &app.App{Messages: m, Sessions: s}
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, dataDir, sess.ID, &buf))

	out := buf.String()
	require.Contains(t, out, "status: running",
		"an unreadable lock is possibly live (fail-open, ASYNC-02)")
	require.Contains(t, out, "could not be read",
		"the reason must say the lock state could not be read")
	require.NotContains(t, out, "status: at rest",
		"fail-open must not collapse to 'at rest'")

	// Control: a genuinely absent lock file (ENOENT) must still print
	// "status: at rest".
	var buf2 bytes.Buffer
	require.NoError(t, explainWhy(a, t.TempDir(), sess.ID, &buf2))
	require.Contains(t, buf2.String(), "status: at rest",
		"verifiable absence (ENOENT) must say 'at rest'")
}
