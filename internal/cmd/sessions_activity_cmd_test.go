package cmd

// The one-table oracle for R-ACT-2 (task item 4): a single table of fact
// shapes, each row asserted against EVERY reader -- the STATUS `sessions
// list` shows (listStatus over the classifier verdict), the first line
// `sessions why` prints, inject's running/status (kindIsLive), the
// watch/tail follow predicate (tailSessionFinished), and the locks
// between-turns/prune guard (kindIsLive over the batch result's LockStems
// reverse map). One row per shape means two readers can never disagree
// again without failing the same line.
//
// Revert-check mutant: re-introducing ANY pre-classifier local decision
// (a lock-only running check in inject, an mtime-based list status, a
// per-reader live-work aggregation) breaks exactly the rows whose shape the
// local rule misjudged -- e.g. an mtime rule reads shape "released" (fresh
// empty lock) as running and fails its list/inject/tail cells.

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Salvaged helpers (the deleted sessions_livework_test.go's shared
// seams), now feeding the classifier-level tests.

func dbConnectForTest(t *testing.T, dataDir string) (*sql.DB, error) {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	if err == nil {
		t.Cleanup(func() { _ = db.Release(dataDir) })
	}
	return conn, err
}

func newDeadHostStore(t *testing.T, dataDir string, pid int, label string) *session.AsyncJobStore {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	st := session.NewAsyncJobStore(conn, dataDir, pid, label)
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

// ageLock back-dates a lock file's mtime by d. Display-only after D10:
// no reader decision may depend on it.
func ageLock(t *testing.T, path string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	require.NoError(t, os.Chtimes(path, old, old))
}

func releasedLock(t *testing.T, dataDir, sessionID string) string {
	t.Helper()
	path := writeLockFileAt(t, dataDir, sessionID, 0)
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	backDateLock(t, path)
	return path
}

func tailDone(ctx context.Context, a *app.App, sessionID string) bool {
	done, _ := tailSessionFinished(ctx, a, sessionID)
	return done
}

// newActivityCmdApp builds a lightweight App with the real SQLite stores on
// a temp data dir -- the shape every sessions_* command test drives.
func newActivityCmdApp(t *testing.T) (*app.App, string, *session.AsyncJobStore) {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := dbConnectForTest(t, dataDir)
	require.NoError(t, err)
	q := db.New(conn)
	a := &app.App{
		Sessions: session.NewService(q, conn),
		Messages: message.NewService(q),
		DB:       func() *sql.DB { return conn },
	}
	a.SetDataDirForTest(dataDir)
	store := session.NewAsyncJobStore(conn, dataDir, os.Getpid(), "activity-cmd-test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	a.SetAsyncJobStoreForTest(store)
	a.SetWakeScheduleStoreForTest(session.NewWakeScheduleStore(conn))
	return a, dataDir, store
}

// TestSessionsActivity_OneTableEveryCommand is the cross-command oracle.
func TestSessionsActivity_OneTableEveryCommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, dataDir, store := newActivityCmdApp(t)

	mk := func(name string) session.Session {
		sess, err := a.Sessions.Create(ctx, name)
		require.NoError(t, err)
		return sess
	}

	// --- fact shapes -----------------------------------------------------

	// in turn: the lock record names this (alive) process.
	inTurn := mk("in-turn")
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, "locks"), 0o755))
	heldPath := filepath.Join(dataDir, "locks", "session-"+session.SessionLockStem(inTurn.ID)+".lock")
	require.NoError(t, os.WriteFile(heldPath, []byte(itoaCmd(os.Getpid())+"\n"), 0o644))

	// between turns (R6C-1/R5C-5): released lock, live driver, debt pending.
	between := mk("between")
	require.NoError(t, store.ClaimSessionDriver(ctx, between.ID))
	require.NoError(t, store.InsertSessionNotice(ctx, between.ID, "wake", "x", true, ""))
	releasedLock(t, dataDir, between.ID)

	// delegating (D2): a live delegation outranks everything else. The row
	// is claimed on the store's own (alive) host.
	delegating := mk("delegating")
	require.NoError(t, store.ClaimSessionDriver(ctx, delegating.ID))
	child := mk("delegating-child")
	deadline := time.Now().Add(time.Hour)
	_, err := store.Claim(ctx, session.ClaimParams{
		Owner: delegating.ID, ToolCallID: "call-1", Kind: session.JobKindAgent,
		Input: "delegating fixture", ChildSessionID: child.ID,
		Deadline: &deadline, TimeoutKind: "wake_only",
	})
	require.NoError(t, err)

	// crashed (D3): a dead foreign PID and NO live work, whatever the finish.
	crashed := mk("crashed")
	require.NoError(t, a.Sessions.SetEndedReason(ctx, crashed.ID, "end_turn"))
	crashedPath := filepath.Join(dataDir, "locks", "session-"+session.SessionLockStem(crashed.ID)+".lock")
	require.NoError(t, os.WriteFile(crashedPath, []byte("2147483646\n"), 0o644))

	// ended (D4): the recorded reason is the only end signal.
	ended := mk("ended")
	require.NoError(t, a.Sessions.SetEndedReason(ctx, ended.ID, "end_turn"))

	// idle: nothing at all.
	idle := mk("idle")

	ids := []string{inTurn.ID, between.ID, delegating.ID, crashed.ID, ended.ID, idle.ID}
	for _, id := range ids {
		addFinishedAssistant(t, a.Messages, id, message.FinishReasonEndTurn)
	}
	acts, err := a.SessionActivityBatch(ctx, ids)
	require.NoError(t, err)
	for _, id := range ids {
		require.Empty(t, acts.ByID[id].Facts.Unreadable, "no fact read may fail in this fixture (%s)", id)
	}

	type expect struct {
		status    string // list STATUS word ("" = at rest)
		whyStatus string // `sessions why` first line
		live      bool   // inject running / locks between-turns guard / watch keeps
	}
	table := map[string]expect{
		inTurn.ID:     {status: "running", whyStatus: "status: running", live: true},
		between.ID:    {status: "running", whyStatus: "status: running", live: true},
		delegating.ID: {status: "delegating", whyStatus: "status: delegating", live: true},
		crashed.ID:    {status: "crashed", whyStatus: "status: crashed", live: false},
		ended.ID:      {status: "done", whyStatus: "status: done", live: false},
		idle.ID:       {status: "", whyStatus: "status: at rest", live: false},
	}

	for _, id := range ids {
		want := table[id]
		// list cell
		require.Equal(t, want.status, listStatus(acts.ByID[id].Verdict), id)
		// why first line
		var buf strings.Builder
		require.NoError(t, explainSessionStatus(ctx, a, dataDir, id, &buf))
		require.True(t, strings.HasPrefix(buf.String(), want.whyStatus+"\n"),
			"%s: why output %q does not start with %q", id, buf.String(), want.whyStatus)
		// inject running / watch-tail predicate / locks guard: one verdict
		require.Equal(t, want.live, kindIsLive(acts.ByID[id].Verdict.Kind), id)
		// watch/tail follow predicate: finished <=> no live verdict
		require.Equal(t, !want.live, tailDone(ctx, a, id), id)
		// locks stem map (D11): the real id is reachable from its stem
		stem := session.SessionLockStem(id)
		require.Contains(t, acts.LockStems[stem], id, id)
	}

	// The dead-PID lock of the crashed session must never read as live in
	// the locks guard, even aged (the pre-classifier mtime rule said live
	// for a fresh empty lock -- D10's regression).
	require.False(t, kindIsLive(acts.ByID[crashed.ID].Verdict.Kind))
}

func itoaCmd(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
