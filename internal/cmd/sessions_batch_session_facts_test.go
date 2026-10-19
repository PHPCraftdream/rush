package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestExplainWhyBatchPreservesClassifierWithoutBatchWork(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	a, wake, sessID, _ := wakeWhyFixture(t, time.Hour)
	dataDir := t.TempDir()
	a.SetDataDirForTest(dataDir)
	ctx := context.Background()
	_, err := session.NewBatchStore(a.DB()).Reserve(ctx, session.BatchReservation{
		Owner: sessID, Host: "facts-host", RunnerID: "facts-runner", ToolCallID: "facts-call", InputHash: "facts-hash", Sequence: 1,
		Root: session.BatchReservationNode{Values: db.ReserveBatchNodeParams{Kind: "batch", Mode: sql.NullString{String: "parallel", Valid: true}, State: "running", ControlState: "none"}},
	})
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sessID, &out))
	require.Equal(t, "status: batch live", strings.SplitN(out.String(), "\n", 2)[0])
	open, err := session.OpenOnceWakeSchedules(ctx, wake, sessID)
	require.NoError(t, err)
	require.Len(t, open, 1, "the batch verdict must not consume the open once schedule")
}

func TestExplainWhyBatchUnknownDominatesOnceSchedule(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	ctx := context.Background()
	a, _, sessID, _ := wakeWhyFixture(t, time.Hour)
	dataDir := writeLockFile(t, sessID, 99999999)
	a.SetDataDirForTest(dataDir)
	a.DB = func() *sql.DB { return nil }
	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sessID, &out))
	lines := strings.SplitN(out.String(), "\n", 3)
	require.Equal(t, "status: batch unknown (incomplete read)", lines[0])
	require.Contains(t, lines[1], "reason: the batch read was incomplete")
}

func TestExplainWhyBatchDriver(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	ctx := context.Background()
	a, s, _, store, dataDir := newRunDriverTestApp(t)
	sess, err := s.Create(ctx, "batch driver")
	require.NoError(t, err)
	require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))
	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &out))
	require.Contains(t, out.String(), "status: running")
	require.Contains(t, out.String(), "driver: a `rush run` loop")
	require.NoError(t, store.ReleaseSessionDriver(ctx, sess.ID))
	out.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sess.ID, &out))
	require.Contains(t, out.String(), "status: at rest")
}

func TestSessionQuestionReaderForAppSelectsBatchReader(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "1")
	testApp, _, _, _, _ := newRunDriverTestApp(t)
	reader := sessionQuestionReaderForApp(testApp)
	require.NotNil(t, reader)
	_, ok := reader.(*session.SessionNoticeStore)
	require.True(t, ok)
	requireNilReader(t, sessionQuestionReaderForApp(nil))
	noDB := &app.App{DB: func() *sql.DB { return nil }}
	requireNilReader(t, sessionQuestionReaderForApp(noDB))
}

func TestSessionQuestionReaderForAppOffNilStore(t *testing.T) {
	t.Setenv("RUSH_BATCH_RUNTIME", "false")
	a := &app.App{}
	requireNilReader(t, sessionQuestionReaderForApp(a))
	store := session.NewAsyncJobStore(nil, "", 1, "test")
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	a.SetAsyncJobStoreForTest(store)
	reader := sessionQuestionReaderForApp(a)
	got, ok := reader.(*session.AsyncJobStore)
	require.True(t, ok)
	require.Same(t, store, got)
}

// requireNilReader asserts the interface value itself is nil. testify's Nil
// also accepts a typed-nil pointer inside a non-nil interface, which is
// exactly the regression (listStatusesWithAwaiting checks `store != nil`).
func requireNilReader[T any](t *testing.T, reader T) {
	t.Helper()
	if any(reader) != nil {
		t.Fatalf("question reader must be a nil interface, got %T", reader)
	}
}
