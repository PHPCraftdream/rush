// Stage 5a: the `sessions` readers' wake-schedule awareness. `why` names an
// open once schedule and refuses done/at-rest; `list`'s promotion and the
// shared classifier report the session as live; `cancel` takes
// the schedules down so the waiting run is not orphaned on its timer.
package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// wakeWhyFixture builds an App (messages + sessions + wake store) with one
// session whose last assistant message finished cleanly and one ACTIVE once
// schedule due in the given delay -- the "run ended its turn, the timer
// holds it" shape.
func wakeWhyFixture(t *testing.T, in time.Duration) (*app.App, *session.WakeScheduleStore, string, string) {
	t.Helper()
	conn, q := newSessionFactsTestDB(t)
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

	a := &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetWakeScheduleStoreForTest(wake)
	return a, wake, sess.ID, row.ID
}

// (е) `sessions why` names the open schedule and reports the session as
// running, not done/at rest.
func TestExplainSessionStatus_OpenWakeScheduleHoldsSessionOpen(t *testing.T) {
	a, _, sessionID, scheduleID := wakeWhyFixture(t, time.Hour)

	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, t.TempDir(), sessionID, &buf))
	out := buf.String()
	require.Contains(t, out, "status: running")
	require.Contains(t, out, "wake schedule "+scheduleID)
	require.Contains(t, out, "waiting on")
}

// A session whose only schedule is a loop is NOT held open (the CLI cancels
// loop schedules at close, so they are not live work).
func TestExplainSessionStatus_LoopScheduleIsNotLiveWork(t *testing.T) {
	conn, q := newSessionFactsTestDB(t)
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

	a := &app.App{Messages: m, Sessions: s, DB: func() *sql.DB { return conn }}
	a.SetWakeScheduleStoreForTest(wake)
	var buf bytes.Buffer
	require.NoError(t, explainWhy(a, t.TempDir(), sess.ID, &buf))
	require.Contains(t, buf.String(), "status: at rest")
}

// The classifier sees the once schedule: the session is between turns and
// waits on it (watch keeps following; cancel --all flags; reset refuses) --
// the classifier shared
// fact, now read once.
func TestSessionActivity_OpenOnceScheduleIsBetweenTurns(t *testing.T) {
	a, _, sessionID, scheduleID := wakeWhyFixture(t, time.Hour)

	act, err := a.SessionActivity(context.Background(), sessionID)
	require.NoError(t, err)
	require.Empty(t, act.Facts.Unreadable)
	require.Equal(t, session.ActivityBetweenTurns, act.Verdict.Kind)
	require.Contains(t, act.Verdict.WaitingOn, session.WaitSchedule)
	require.Contains(t, act.Verdict.Description, "wake schedule "+scheduleID)
	require.Equal(t, "running", listStatus(act.Verdict))
	require.True(t, sessionHasLiveWork(context.Background(), a, "", sessionID))
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
