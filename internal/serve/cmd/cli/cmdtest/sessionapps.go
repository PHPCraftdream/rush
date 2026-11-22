package cmdtest

// Shared session-test App fixtures relocated (AST copies, names
// upper-cased) from internal/serve/cmd's root _test files: the lightweight
// real-sqlite App builders, the finished-assistant message seeder and the
// delegation claimer. The originals stay until the test-port workers
// delete them.

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// DBConnectForTest opens the migrated sqlite DB for dataDir and registers
// the pooled-connection release on cleanup. Copy of dbConnectForTest
// (sessions_activity_cmd_test.go).
func DBConnectForTest(t *testing.T, dataDir string) (*sql.DB, error) {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	if err == nil {
		t.Cleanup(func() { _ = db.Release(dataDir) })
	}
	return conn, err
}

// NewDeadHostStore opens a HostStore on dataDir for a (possibly dead) pid,
// registering both connection release and store close on cleanup. Copy of
// newDeadHostStore (sessions_activity_cmd_test.go).
func NewDeadHostStore(t *testing.T, dataDir string, pid int, label string) *sessionhosts.HostStore {
	t.Helper()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	st := sessionhosts.NewHostStore(conn, dataDir, pid, label)
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st
}

// NewActivityCmdApp builds a lightweight App with the real SQLite stores on
// a temp data dir -- the shape every sessions_* command test drives. The
// returned store is the host store the driver markers are claimed through.
// Copy of newActivityCmdApp (sessions_activity_cmd_test.go).
func NewActivityCmdApp(t *testing.T) (*app.App, string, *sessionhosts.HostStore) {
	t.Helper()
	dataDir := t.TempDir()
	conn, err := DBConnectForTest(t, dataDir)
	require.NoError(t, err)
	q := db.New(conn)
	a := &app.App{
		Sessions: coresession.NewService(q, conn),
		Messages: message.NewService(q),
		DB:       func() *sql.DB { return conn },
	}
	a.SetDataDirForTest(dataDir)
	store := sessionhosts.NewHostStore(conn, dataDir, os.Getpid(), "activity-cmd-test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	a.SetWakeScheduleStoreForTest(sessionwake.NewWakeScheduleStore(conn))
	return a, dataDir, store
}

// ItoaCmd formats n in base 10 without allocations. Copy of itoaCmd
// (sessions_activity_cmd_test.go).
func ItoaCmd(n int) string {
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

// AddFinishedAssistant records a finished assistant message on sessionID
// with the given finish reason -- the message-store half of the "clean
// finish" reclassification rule. Copy of addFinishedAssistant
// (sessions_why_descendant_test.go).
func AddFinishedAssistant(t *testing.T, m message.Service, sessionID string, reason message.FinishReason) {
	t.Helper()
	assistant, err := m.Create(context.Background(), sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "delegated the work"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(reason, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))
}

// NewWhyDescendantTestApp builds a lightweight *app.App (session + message
// services over a REAL, fully migrated sqlite DB -- db.Connect, not a
// hand-rolled schema, since the batch readers need batch_nodes and
// session_notices to exist -- no full agent coordinator). Copy of
// newWhyDescendantTestApp (sessions_why_descendant_test.go).
func NewWhyDescendantTestApp(t *testing.T) (a *app.App, s coresession.Service, m message.Service, store *session.SessionNoticeStore, dataDir string) {
	t.Helper()
	dataDir = t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q := db.New(conn)
	s = coresession.NewService(q, conn)
	m = message.NewService(q)
	a = &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetDataDirForTest(dataDir)
	return a, s, m, session.NewSessionNoticeStore(conn), dataDir
}

// BatchSeedHost registers the fixture's own (in-process, alive) host and
// returns its id plus the batch store, the pair the delegation seeders
// reserve through. Copy of batchSeedHost (sessions_own_job_status_test.go);
// needed here because ClaimDelegation calls it.
func BatchSeedHost(t *testing.T, a *app.App) (string, *session.BatchStore) {
	t.Helper()
	store := sessionhosts.NewHostStore(a.DB(), a.DataDirectory(), os.Getpid(), "batch-seed-fixture")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	host, err := store.EnsureHost(context.Background())
	require.NoError(t, err)
	return host, session.NewBatchStore(a.DB())
}

// ClaimDelegation seeds the "parentID is waiting on childID" fact: a live
// batch root whose task child carries the ChildSessionID link (set directly
// in the reservation's child Values -- db.ReserveBatchNodeParams has the
// field). The root stays live as long as it stays nonterminal on a host
// this reader judges alive (the fixture's own registered host is always
// alive-without-probing in-process). Copy of claimDelegation
// (sessions_why_descendant_test.go).
func ClaimDelegation(t *testing.T, a *app.App, parentID, toolCallID, childID string) {
	t.Helper()
	host, bs := BatchSeedHost(t, a)
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
