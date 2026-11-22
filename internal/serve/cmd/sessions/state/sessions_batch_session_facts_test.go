package sessstate

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	sessbatch "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/report/batch"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/stretchr/testify/require"
)

func TestExplainSessionStatus_OpenWakeScheduleHoldsSessionOpen(t *testing.T) {
	a, _, sessID, scheduleID := wakeWhyFixture(t, time.Hour)
	dataDir := t.TempDir()
	a.SetDataDirForTest(dataDir)
	var out bytes.Buffer
	err := sessbatch.ExplainWhyBatch(context.Background(), a, dataDir, sessID, &out, printStallHeader)
	require.NoError(t, err)
	require.Contains(t, out.String(), "status: running")
	require.Contains(t, out.String(), "wake schedule "+scheduleID)
	require.Contains(t, out.String(), "waiting on")
}

func TestExplainSessionStatus_LoopScheduleIsNotLiveWork(t *testing.T) {
	a, wake, sessID, onceID := wakeWhyFixture(t, time.Hour)
	ctx := context.Background()
	require.NoError(t, wake.CancelSchedule(ctx, sessID, onceID, time.Now()))
	_, err := wake.CreateSchedule(ctx, sessionwake.CreateWakeScheduleParams{
		Owner: sessID, Kind: sessionwake.WakeKindLoop, Message: "tick",
		RunAt: time.Now().Add(time.Hour), Every: 5 * time.Minute,
	}, time.Now())
	require.NoError(t, err)
	dataDir := t.TempDir()
	a.SetDataDirForTest(dataDir)
	var out bytes.Buffer
	err = sessbatch.ExplainWhyBatch(ctx, a, dataDir, sessID, &out, printStallHeader)
	require.NoError(t, err)
	require.Contains(t, out.String(), "status: at rest")
}

func TestExplainWhyBatchPreservesClassifierWithoutBatchWork(t *testing.T) {
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
	open, err := sessionwake.OpenOnceWakeSchedules(ctx, wake, sessID)
	require.NoError(t, err)
	require.Len(t, open, 1, "the batch verdict must not consume the open once schedule")
}

func TestExplainWhyBatchUnknownDominatesOnceSchedule(t *testing.T) {
	ctx := context.Background()
	a, _, sessID, _ := wakeWhyFixture(t, time.Hour)
	dataDir := t.TempDir()
	cmdtest.WriteLockFileAt(t, dataDir, sessID, 99999999)
	a.SetDataDirForTest(dataDir)
	a.DB = func() *sql.DB { return nil }
	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, dataDir, sessID, &out))
	lines := strings.SplitN(out.String(), "\n", 3)
	require.Equal(t, "status: batch unknown (incomplete read)", lines[0])
	require.Contains(t, lines[1], "reason: the batch read was incomplete")
}

func TestExplainWhyBatchDriver(t *testing.T) {
	a, _, store := stateRunDriverTestApp(t)
	ctx := context.Background()
	sess, err := a.Sessions.Create(ctx, "batch driver")
	require.NoError(t, err)
	require.NoError(t, store.ClaimSessionDriver(ctx, sess.ID))
	var out bytes.Buffer
	require.NoError(t, explainSessionStatus(ctx, a, a.DataDirectory(), sess.ID, &out))
	require.Contains(t, out.String(), "status: running")
	require.Contains(t, out.String(), "driver: a `rush run` loop")
	require.NoError(t, store.ReleaseSessionDriver(ctx, sess.ID))
	out.Reset()
	require.NoError(t, explainSessionStatus(ctx, a, a.DataDirectory(), sess.ID, &out))
	require.Contains(t, out.String(), "status: at rest")
}

func TestSessionQuestionReaderForAppSelectsNoticeStore(t *testing.T) {
	testApp, _, _ := stateRunDriverTestApp(t)
	reader := sesskit.SessionQuestionReaderForApp(testApp)
	require.NotNil(t, reader)
	_, ok := reader.(*session.SessionNoticeStore)
	require.True(t, ok)
	var nilApp *app.App
	requireNilReader(t, sesskit.SessionQuestionReaderForApp(nilApp))
	noDB := &app.App{DB: func() *sql.DB { return nil }}
	requireNilReader(t, sesskit.SessionQuestionReaderForApp(noDB))
}

// requireNilReader asserts the interface value itself is nil. testify's Nil
// also accepts a typed-nil pointer inside a non-nil interface, which is
// exactly the regression (ListStatusesWithAwaiting checks `store != nil`).
func requireNilReader[T any](t *testing.T, reader T) {
	t.Helper()
	if any(reader) != nil {
		t.Fatalf("question reader must be a nil interface, got %T", reader)
	}
}
