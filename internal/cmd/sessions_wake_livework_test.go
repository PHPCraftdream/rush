// Stage 5a: the `sessions` readers' wake-schedule awareness. `why` names an
// open once schedule and refuses done/at-rest; `list`'s promotion and the
// shared inspectSessionLiveWork report the session as live; `cancel` takes
// the schedules down so the waiting run is not orphaned on its timer.
package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// newWakeTestDB is newTestDB plus the wake_schedules table (the cmd-level
// test schema predates stage 4a; same columns as migration
// 20260929000005_add_wake_schedules.sql).
func newWakeTestDB(t *testing.T) (*sql.DB, *db.Queries) {
	t.Helper()
	conn, q := newTestDB(t)
	_, err := conn.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS wake_schedules (
			id               TEXT PRIMARY KEY,
			owner_session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
			kind             TEXT NOT NULL CHECK (kind IN ('once', 'loop')),
			message          TEXT NOT NULL,
			next_run_at      INTEGER NOT NULL,
			every_ms         INTEGER NOT NULL DEFAULT 0,
			max_runs         INTEGER,
			until_at         INTEGER,
			state            TEXT NOT NULL CHECK (state IN ('active', 'done', 'cancelled')),
			occurrence       INTEGER NOT NULL DEFAULT 0,
			lease_owner      TEXT,
			lease_expires_at INTEGER,
			created_at       INTEGER NOT NULL,
			updated_at       INTEGER NOT NULL
		);
	`)
	require.NoError(t, err)
	return conn, q
}

// wakeWhyFixture builds an App (messages + sessions + wake store) with one
// session whose last assistant message finished cleanly and one ACTIVE once
// schedule due in the given delay -- the "run ended its turn, the timer
// holds it" shape.
func wakeWhyFixture(t *testing.T, in time.Duration) (*app.App, *session.WakeScheduleStore, string, string) {
	t.Helper()
	conn, q := newWakeTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	wake := session.NewWakeScheduleStore(conn)

	sess, err := s.Create(context.Background(), "waiting on timer")
	require.NoError(t, err)
	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "waiting"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))

	row, err := wake.CreateSchedule(context.Background(), session.CreateWakeScheduleParams{
		Owner: sess.ID, Kind: session.WakeKindOnce, Message: "timer", RunAt: time.Now().Add(in),
	}, time.Now())
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}
	a.SetWakeScheduleStoreForTest(wake)
	return a, wake, sess.ID, row.ID
}

// (е) `sessions why` names the open schedule and reports the session as
// running, not done/at rest.
func TestExplainSessionStatus_OpenWakeScheduleHoldsSessionOpen(t *testing.T) {
	a, _, sessionID, scheduleID := wakeWhyFixture(t, time.Hour)

	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, t.TempDir(), sessionID, &buf))
	out := buf.String()
	require.Contains(t, out, "status: running")
	require.Contains(t, out, "wake schedule "+scheduleID)
	require.Contains(t, out, "waiting on")
}

// A session whose only schedule is a loop is NOT held open (the CLI cancels
// loop schedules at close, so they are not live work).
func TestExplainSessionStatus_LoopScheduleIsNotLiveWork(t *testing.T) {
	conn, q := newWakeTestDB(t)
	s := session.NewService(q, conn)
	m := message.NewService(q)
	wake := session.NewWakeScheduleStore(conn)
	sess, err := s.Create(context.Background(), "loop done")
	require.NoError(t, err)
	assistant, err := m.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "done"}},
	})
	require.NoError(t, err)
	assistant.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, m.Update(context.Background(), assistant))
	_, err = wake.CreateSchedule(context.Background(), session.CreateWakeScheduleParams{
		Owner: sess.ID, Kind: session.WakeKindLoop, Message: "tick",
		RunAt: time.Now().Add(time.Hour), Every: 5 * time.Minute,
	}, time.Now())
	require.NoError(t, err)

	a := &app.App{Messages: m, Sessions: s}
	a.SetWakeScheduleStoreForTest(wake)
	var buf bytes.Buffer
	require.NoError(t, explainSessionStatus(context.Background(), a, t.TempDir(), sess.ID, &buf))
	require.Contains(t, buf.String(), "status: at rest")
}

// inspectSessionLiveWork sees the once schedule (watch/cancel --all/reset's
// refusal all read .active() through it).
func TestInspectSessionLiveWork_OpenOnceScheduleIsActive(t *testing.T) {
	a, _, sessionID, scheduleID := wakeWhyFixture(t, time.Hour)

	w := inspectSessionLiveWork(context.Background(), a, sessionID)
	require.True(t, w.active())
	require.Len(t, w.wakeSchedules, 1)
	require.Equal(t, scheduleID, w.wakeSchedules[0].ID)
	require.Contains(t, w.describe(), "waiting on wake schedule "+scheduleID)
}

// markOpenWakeSchedules promotes done/at-rest to running for the list view.
func TestMarkOpenWakeSchedules_PromotesHeldSessions(t *testing.T) {
	a, _, sessionID, _ := wakeWhyFixture(t, time.Hour)
	sess, err := a.Sessions.Get(context.Background(), sessionID)
	require.NoError(t, err)

	got := markOpenWakeSchedules(context.Background(), a, []session.Session{sess}, map[string]string{sessionID: "done"})
	require.Equal(t, "running", got[sessionID])
	// Never downgrades a stronger verdict.
	got = markOpenWakeSchedules(context.Background(), a, []session.Session{sess}, map[string]string{sessionID: "crashed"})
	require.Equal(t, "crashed", got[sessionID])
}

// cancelWakeSchedules takes every ACTIVE schedule down (once and loop), so
// `sessions cancel` cannot orphan a run on its timer.
func TestCancelWakeSchedules_CancelsActive(t *testing.T) {
	a, wake, sessionID, _ := wakeWhyFixture(t, time.Hour)
	_, err := wake.CreateSchedule(context.Background(), session.CreateWakeScheduleParams{
		Owner: sessionID, Kind: session.WakeKindLoop, Message: "tick",
		RunAt: time.Now().Add(time.Hour), Every: 5 * time.Minute,
	}, time.Now())
	require.NoError(t, err)

	n := cancelWakeSchedules(context.Background(), a, sessionID)
	require.EqualValues(t, 2, n)
	open, err := session.OpenOnceWakeSchedules(context.Background(), wake, sessionID)
	require.NoError(t, err)
	require.Empty(t, open)
	active, err := wake.CountActive(context.Background(), sessionID)
	require.NoError(t, err)
	require.Zero(t, active)
}
