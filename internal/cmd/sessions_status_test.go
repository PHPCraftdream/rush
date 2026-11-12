package cmd

// Tests for `rush sessions status`: the one-shot machine truth snapshot.
// They drive the testable core (sessionsStatusSnapshot) directly with
// hand-built Apps, the same way tests drive explainWhy, and assert on the
// sessionStatusReport struct — including the honesty rule: unreadable or
// absent evidence is the literal "unknown", and a recorded ended_reason is
// never misreported as proof of physical process exit.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// sessionsStatusTestApp wires a hand-built App over the shared facts-test
// database the other sessions_* tests use, pointing the lock reader at
// dataDir.
func sessionsStatusTestApp(t *testing.T, dataDir string) (*app.App, session.Service) {
	t.Helper()
	conn, q := newSessionFactsTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	a := &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetDataDirForTest(dataDir)
	return a, s
}

// TestSessionsStatus_TurnHeldLock: a session lock held by a provably live
// PID (our own) is phase "turn" with process.alive exactly true from the
// real OS liveness probe, no driver PID, and — with a complete batch read
// and no roots at all — batch.terminal exactly true.
//
// Revert-check: treating a held lock as unknown, or consuming absent/ended
// evidence as death, fails the Alive, phase and sources assertions.
func TestSessionsStatus_TurnHeldLock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	a, s := sessionsStatusTestApp(t, dataDir)
	sess, err := s.Create(ctx, "status held lock")
	require.NoError(t, err)

	dataDir = writeLockFile(t, sess.ID, os.Getpid())
	a.SetDataDirForTest(dataDir)

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, sess.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, "turn", rep.Phase)
	require.Equal(t, true, rep.Process.Alive)
	require.Nil(t, rep.Process.DriverPID)
	require.Contains(t, rep.Process.Evidence, "OS liveness probe")
	require.Contains(t, rep.Sources, "session_lock")
	require.Contains(t, rep.Sources, "session_db")
	require.Equal(t, true, rep.Batch.Terminal,
		"a complete read with zero roots is terminal: nothing pending")
	require.Empty(t, rep.Batch.Roots)
}

// TestSessionsStatus_EndedReasonIsNotProcessExit: a session whose only end
// evidence is a recorded ended_reason (no lock file, no driver marker,
// empty session) is phase "finished" with the exit reason carried, but
// process.alive MUST stay the literal string "unknown": a database fact is
// not proof a process exited.
//
// Revert-check: deriving Alive=false from ended_reason or lock absence
// alone — the exact misreport this command exists to prevent — fails the
// Alive and evidence assertions.
func TestSessionsStatus_EndedReasonIsNotProcessExit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir() // no locks dir: no lock file, no driver marker
	a, s := sessionsStatusTestApp(t, dataDir)
	sess, err := s.Create(ctx, "status ended reason")
	require.NoError(t, err)
	require.NoError(t, s.SetEndedReason(ctx, sess.ID, "end_turn"))

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, sess.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, "finished", rep.Phase)
	require.Equal(t, "end_turn", rep.ExitReason)
	require.Equal(t, "unknown", rep.Process.Alive,
		"a recorded ended_reason is a database fact, not proof of physical exit")
	require.NotEqual(t, false, rep.Process.Alive)
	require.NotEqual(t, true, rep.Process.Alive)
	require.Contains(t, rep.Process.Evidence, "not proof")
}

// TestSessionsStatus_CrashedDeadPIDIsProvenFalse: a lock recording a
// provably dead PID (999999) is a REAL probe result: phase "crashed" and
// process.alive exactly false, with the evidence naming the PID and saying
// the process is not alive.
//
// Revert-check: reporting a probed-dead holder as alive or as mere unknown
// fails the Alive and evidence assertions.
func TestSessionsStatus_CrashedDeadPIDIsProvenFalse(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	a, s := sessionsStatusTestApp(t, t.TempDir())
	sess, err := s.Create(ctx, "status dead pid")
	require.NoError(t, err)

	dataDir := writeLockFile(t, sess.ID, 999999)
	a.SetDataDirForTest(dataDir)

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, sess.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, "crashed", rep.Phase)
	require.Equal(t, false, rep.Process.Alive)
	require.Nil(t, rep.Process.DriverPID)
	require.Contains(t, rep.Process.Evidence, "999999")
	require.Contains(t, rep.Process.Evidence, "not alive")
}

// TestSessionsStatus_LiveDelegationWaiting: a live batch delegation with a
// linked child session is phase "waiting" with the delegation counted as a
// live child, the sub-session walk seeing the child, and the held root
// making the batch verdict nonterminal.
//
// Revert-check: unwiring the batch live read (Terminal would flip to true)
// or the sub-session walk (Total would go nil or miss the child) fails
// these assertions.
func TestSessionsStatus_LiveDelegationWaiting(t *testing.T) {
	t.Parallel()

	a, _, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()
	parent, err := a.Sessions.Create(ctx, "status delegation root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "status-delegation-child", parent.ID, "worker")
	require.NoError(t, err)
	seedCmdBatchDelegation(t, a.DB(), parent.ID, "status-fixture-host", "status-delegate-1", child.ID)

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, parent.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, "waiting", rep.Phase,
		"a live sub-agent delegation is the waiting phase")
	require.GreaterOrEqual(t, rep.Children.Live, 1)
	require.NotNil(t, rep.Children.Total)
	require.GreaterOrEqual(t, *rep.Children.Total, 1,
		"the sub-session walk must see the linked child session")
	require.Equal(t, false, rep.Batch.Terminal)
	require.GreaterOrEqual(t, len(rep.Batch.Roots), 1)
	require.True(t, rep.Batch.Roots[0].Held)
	require.Contains(t, rep.Sources, "batch_nodes")
	require.Contains(t, rep.Sources, "sub_sessions")
}

// TestSessionsStatus_HeldQuestionCounted: a pending child_question notice
// on a live delegation is counted in held_questions as exactly 1.
//
// Revert-check: dropping the PendingChildQuestions read silently zeroes the
// count (or nils it) and fails the assertion.
func TestSessionsStatus_HeldQuestionCounted(t *testing.T) {
	t.Parallel()

	a, store, dataDir := whyAwaitingTestEnv(t)
	ctx := context.Background()
	parent, err := a.Sessions.Create(ctx, "status question root")
	require.NoError(t, err)
	child, err := a.Sessions.CreateTaskSession(ctx, "status-question-child", parent.ID, "worker")
	require.NoError(t, err)
	seedCmdBatchDelegation(t, a.DB(), parent.ID, "status-fixture-host", "status-delegate-1", child.ID)
	insertChildQuestionNotice(t, store, parent.ID, child.ID, "status-delegate-1", "A or B?")

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, parent.ID, time.Now())
	require.NoError(t, err)
	require.NotNil(t, rep.Children.HeldQuestions,
		"a successful notice read is a count (possibly zero), never nil")
	require.Equal(t, 1, *rep.Children.HeldQuestions)
}

// TestSessionsStatus_HonestUnknownOnNoEvidence: a fresh session with no
// lock, no messages and no ended reason reads phase "idle",
// process.alive "unknown", nil activity age, nil last tool — and children
// total exactly 0: the walk SUCCEEDED and found nothing, which is not the
// same as the walk having failed.
//
// Revert-check: defaulting Alive to false (claiming exit without evidence)
// or Total to nil fails these assertions.
func TestSessionsStatus_HonestUnknownOnNoEvidence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir() // no locks dir: no heartbeat pulse either
	a, s := sessionsStatusTestApp(t, dataDir)
	sess, err := s.Create(ctx, "status fresh idle")
	require.NoError(t, err)

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, sess.ID, time.Now())
	require.NoError(t, err)
	require.Equal(t, "idle", rep.Phase)
	require.Equal(t, "unknown", rep.Process.Alive,
		"an absent lock and no driver prove nothing about the process")
	require.Nil(t, rep.SecondsSinceActivity)
	require.Nil(t, rep.LastTool)
	require.NotNil(t, rep.Children.Total)
	require.Equal(t, 0, *rep.Children.Total,
		"a succeeded walk over zero children is a real 0, not a failed nil")
}

// TestSessionsStatus_ConfiglessAppBatchUnknown: a configless App (no DB, no
// Messages, no Sessions) must degrade to unknowns with notes — never panic
// and never produce a definitive terminal answer for unreadable batch
// evidence.
//
// Revert-check: letting a configless read degrade to a definitive
// terminal/idle answer instead of unknown fails the batch assertions.
func TestSessionsStatus_ConfiglessAppBatchUnknown(t *testing.T) {
	t.Parallel()

	a := &app.App{}
	dataDir := t.TempDir()
	a.SetDataDirForTest(dataDir)

	rep, err := sessionsStatusSnapshot(context.Background(), a, dataDir, "configless-id", time.Now())
	require.NoError(t, err)
	require.Equal(t, "unknown", rep.Batch.Terminal)
	require.Empty(t, rep.Batch.Roots)
	require.Equal(t, "idle", rep.Phase, "an absent lock classifies idle, not dead")
	require.Equal(t, "unknown", rep.Process.Alive)
	mentionsDB := false
	for _, n := range rep.Notes {
		if strings.Contains(n, "no database handle") {
			mentionsDB = true
		}
	}
	require.True(t, mentionsDB, "a note must name the missing database handle; notes: %v", rep.Notes)
}

// TestSessionsStatus_TextRender: the text renderer surfaces the process and
// batch lines (the fields humans rely on) plus the phase, children and
// sources lines of the held-lock fixture.
//
// Revert-check: the renderer dropping the process or batch lines fails the
// containment assertions.
func TestSessionsStatus_TextRender(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	a, s := sessionsStatusTestApp(t, dataDir)
	sess, err := s.Create(ctx, "status text render")
	require.NoError(t, err)
	dataDir = writeLockFile(t, sess.ID, os.Getpid())
	a.SetDataDirForTest(dataDir)

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, sess.ID, time.Now())
	require.NoError(t, err)

	var buf bytes.Buffer
	renderSessionsStatusReport(&buf, rep)
	out := buf.String()
	require.Contains(t, out, "phase: turn")
	require.Contains(t, out, "process: alive")
	require.Contains(t, out, "children: live")
	require.Contains(t, out, "batch: terminal")
	require.Contains(t, out, "sources:")
}

// TestSessionsStatus_JSONRoundTrip: the report marshals to the documented
// wire shape (snake_case keys, tri-state booleans intact) and round-trips
// into the generic map an orchestrator script consumes.
//
// Revert-check: a JSON tag rename breaking the documented wire contract
// fails the key and value assertions.
func TestSessionsStatus_JSONRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dataDir := t.TempDir()
	a, s := sessionsStatusTestApp(t, dataDir)
	sess, err := s.Create(ctx, "status json roundtrip")
	require.NoError(t, err)
	dataDir = writeLockFile(t, sess.ID, os.Getpid())
	a.SetDataDirForTest(dataDir)

	rep, err := sessionsStatusSnapshot(ctx, a, dataDir, sess.ID, time.Now())
	require.NoError(t, err)

	raw, err := json.Marshal(rep)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Contains(t, doc, "session_id")
	require.Equal(t, sess.ID, doc["session_id"])
	require.Equal(t, "turn", doc["phase"])
	process, ok := doc["process"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, process["alive"],
		"process.alive must marshal as a JSON boolean, not a string")
	batch, ok := doc["batch"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, batch["terminal"],
		"batch.terminal must marshal as a JSON boolean, not a string")
	require.Contains(t, doc, "seconds_since_activity")
}
