// Wake-schedule snapshot + cancel handlers (stage 5b) — server half of the
// web panel's Schedules tab.
//
// Revert-check: dropping buildSessionWakeSchedules to a hardcoded empty
// payload fails every test asserting a non-empty Schedules list; removing
// the a.Sessions.Get existence check fails
// TestHandleGetSessionWakeSchedules_UnknownSessionIsRefused; removing the
// owner-list membership check in handleCancelWakeSchedule (letting
// CancelSchedule's nil no-op stand for unknown ids) fails
// TestHandleCancelWakeSchedule_UnknownIDIsRefused and
// _ForeignIDIsRefused; dropping the cancelled-state rewrite in
// CancelSchedule (or the fresh-snapshot reply) fails
// TestHandleCancelWakeSchedule_CancelsAndRepliesWithSnapshot.

package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func newWakeSchedulesTestApp(t *testing.T) (*app.App, *Client) {
	t.Helper()
	a := newAttachmentsTestApp(t, t.TempDir(), t.TempDir())
	hub := newHub()
	go hub.Run(t.Context())
	client := newClient(hub, nil)
	client.send = make(chan []byte, 64)
	hub.register <- client
	return a, client
}

// requestWakeSchedules calls handleGetSessionWakeSchedules and returns the
// first EventSessionWakeSchedules reply (or its error text with ok=false).
func requestWakeSchedules(t *testing.T, a *app.App, client *Client, sessionID string) (*SessionWakeSchedulesPayload, string, bool) {
	t.Helper()
	payload, err := json.Marshal(GetSessionWakeSchedulesPayload{SessionID: sessionID})
	require.NoError(t, err)
	handleGetSessionWakeSchedules(t.Context(), a, client, WSMessage{ID: "req", Type: CmdGetSessionWakeSchedules, Payload: payload})
	return readWakeSchedulesReply(t, client)
}

func readWakeSchedulesReply(t *testing.T, client *Client) (*SessionWakeSchedulesPayload, string, bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case raw := <-client.send:
			var env WSMessage
			require.NoError(t, json.Unmarshal(raw, &env))
			switch env.Type {
			case EventSessionWakeSchedules:
				var snap SessionWakeSchedulesPayload
				require.NoError(t, json.Unmarshal(env.Payload, &snap))
				return &snap, "", true
			case EventError:
				return nil, env.Error, false
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("no wake-schedules reply arrived within 2s")
	return nil, "", false
}

func createSchedule(t *testing.T, a *app.App, owner, kind, message string, runAt time.Time, every time.Duration, maxRuns int) string {
	t.Helper()
	store := session.NewWakeScheduleStore(a.DB())
	row, err := store.CreateSchedule(t.Context(), session.CreateWakeScheduleParams{
		Owner: owner, Kind: session.WakeKind(kind), Message: message,
		RunAt: runAt, Every: every, MaxRuns: maxRuns,
	}, time.Now())
	require.NoError(t, err)
	return row.ID
}

func TestHandleGetSessionWakeSchedules_EmptySession(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	sess, err := a.Sessions.Create(t.Context(), "ws empty")
	require.NoError(t, err)

	snap, errText, ok := requestWakeSchedules(t, a, client, sess.ID)
	require.True(t, ok, "unexpected error: %q", errText)
	require.Equal(t, sess.ID, snap.SessionID)
	require.Empty(t, snap.Schedules)
}

func TestHandleGetSessionWakeSchedules_UnknownSessionIsRefused(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	_, errText, ok := requestWakeSchedules(t, a, client, "does-not-exist")
	require.False(t, ok)
	require.NotEmpty(t, errText)
}

func TestHandleGetSessionWakeSchedules_OnceAndLoopWithStates(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	sess, err := a.Sessions.Create(t.Context(), "ws kinds")
	require.NoError(t, err)
	now := time.Now()

	onceID := createSchedule(t, a, sess.ID, "once", "check the build", now.Add(time.Minute), 0, 0)
	loopID := createSchedule(t, a, sess.ID, "loop", "poll the deploy", now.Add(time.Hour), 30*time.Minute, 3)
	// History rows: a cancelled once stays visible to the panel.
	doneID := createSchedule(t, a, sess.ID, "once", "cancelled wake", now.Add(time.Minute), 0, 0)
	store := session.NewWakeScheduleStore(a.DB())
	require.NoError(t, store.CancelSchedule(t.Context(), sess.ID, doneID, now))

	snap, errText, ok := requestWakeSchedules(t, a, client, sess.ID)
	require.True(t, ok, "unexpected error: %q", errText)
	require.Len(t, snap.Schedules, 3)

	byID := map[string]WakeScheduleWire{}
	for _, s := range snap.Schedules {
		byID[s.ID] = s
	}
	once := byID[onceID]
	require.Equal(t, "once", once.Kind)
	require.Equal(t, "active", once.State)
	require.Equal(t, "check the build", once.Message)
	require.Equal(t, int64(0), once.EveryMs)
	require.InDelta(t, now.Add(time.Minute).UnixMilli(), once.NextRunAt, 1500)

	loop := byID[loopID]
	require.Equal(t, "loop", loop.Kind)
	require.Equal(t, "active", loop.State)
	require.Equal(t, int64(3), loop.MaxRuns)
	require.Equal(t, int64(30*time.Minute/time.Millisecond), loop.EveryMs)
	require.Equal(t, int64(0), loop.Occurrence)

	require.Equal(t, "cancelled", byID[doneID].State)
}

// TestHandleGetSessionWakeSchedules_DoneStatesViaClaimAndFire covers the
// "done" state through the store's REAL public path (ClaimDue →
// FireOccurrence, exactly what the stage-4b worker does) — never a direct
// UPDATE — for both terminal shapes: a fired once and a loop that reached
// its max_runs. Revert-check: dropping CompleteOnceWakeSchedule /
// FinishLoopWakeSchedule from FireOccurrence (or the snapshot's state
// mapping) fails the "done" assertions and the occurrence counts.
func TestHandleGetSessionWakeSchedules_DoneStatesViaClaimAndFire(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	sess, err := a.Sessions.Create(t.Context(), "ws done")
	require.NoError(t, err)
	now := time.Now()
	due := now.Add(time.Minute)

	onceID := createSchedule(t, a, sess.ID, "once", "fired once", due, 0, 0)
	// Loop that terminates by max_runs on its SECOND occurrence: the first
	// fire advances (occurrence=1, next_run_at=due+every), the second — at
	// the advanced time — finishes the loop (FinishLoopWakeSchedule marks
	// done WITHOUT another advance).
	loopID := createSchedule(t, a, sess.ID, "loop", "loop till max", due, time.Hour, 2)

	store := session.NewWakeScheduleStore(a.DB())
	// ONE claim sweep leases every due row (the worker's real behavior);
	// firing then goes row by row off that same claim.
	claimed, err := store.ClaimDue(t.Context(), "worker-1", due, 10, session.WakeLeaseTTL)
	require.NoError(t, err)
	claimedIDs := map[string]bool{}
	for _, row := range claimed {
		claimedIDs[row.ID] = true
	}
	require.True(t, claimedIDs[onceID], "the due once schedule must be claimed")
	require.True(t, claimedIDs[loopID], "the due loop schedule must be claimed")
	for _, id := range []string{onceID, loopID} {
		fire, err := store.FireOccurrence(t.Context(), id, "worker-1", due)
		require.NoError(t, err)
		require.True(t, fire.Fired)
	}
	// The once row is done after its single fire; the loop advanced.
	ownerRows, err := store.ListSchedules(t.Context(), sess.ID)
	require.NoError(t, err)
	var loopAdvance *db.WakeSchedule
	for i := range ownerRows {
		if ownerRows[i].ID == loopID {
			loopAdvance = &ownerRows[i]
		}
	}
	require.NotNil(t, loopAdvance, "the loop schedule must exist")
	require.Equal(t, "active", loopAdvance.State)
	require.EqualValues(t, 1, loopAdvance.Occurrence)

	// Second sweep at the loop's ADVANCED due time fires its last occurrence.
	claimed2, err := store.ClaimDue(t.Context(), "worker-1", due.Add(time.Hour), 10, session.WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed2, 1)
	require.Equal(t, loopID, claimed2[0].ID)
	lastLoopFire, err := store.FireOccurrence(t.Context(), loopID, "worker-1", due.Add(time.Hour))
	require.NoError(t, err)
	require.True(t, lastLoopFire.Fired)
	require.True(t, lastLoopFire.Done)

	snap, errText, ok := requestWakeSchedules(t, a, client, sess.ID)
	require.True(t, ok, "unexpected error: %q", errText)
	require.Len(t, snap.Schedules, 2)
	byID := map[string]WakeScheduleWire{}
	for _, s := range snap.Schedules {
		byID[s.ID] = s
	}
	require.Equal(t, "done", byID[onceID].State)
	// The occurrence counter only advances on the loop's ADVANCE path
	// (AdvanceLoopWakeOccurrence); a fired once is marked done without it,
	// and the loop's TERMINAL fire (FinishLoopWakeSchedule) also does not
	// count itself — occurrence 1 = the one advanced occurrence.
	require.Equal(t, int64(0), byID[onceID].Occurrence)
	require.Equal(t, "done", byID[loopID].State)
	require.Equal(t, int64(1), byID[loopID].Occurrence)
}

func TestHandleGetSessionWakeSchedules_ForeignSessionIsolated(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	mine, err := a.Sessions.Create(t.Context(), "ws mine")
	require.NoError(t, err)
	other, err := a.Sessions.Create(t.Context(), "ws other")
	require.NoError(t, err)
	now := time.Now()
	createSchedule(t, a, other.ID, "once", "not yours", now.Add(time.Minute), 0, 0)
	createSchedule(t, a, mine.ID, "once", "mine", now.Add(time.Minute), 0, 0)

	snap, errText, ok := requestWakeSchedules(t, a, client, mine.ID)
	require.True(t, ok, "unexpected error: %q", errText)
	require.Len(t, snap.Schedules, 1)
	require.Equal(t, "mine", snap.Schedules[0].Message)
}

func cancelSchedule(t *testing.T, a *app.App, client *Client, sessionID, scheduleID string) (*SessionWakeSchedulesPayload, string, bool) {
	t.Helper()
	payload, err := json.Marshal(CancelWakeSchedulePayload{SessionID: sessionID, ScheduleID: scheduleID})
	require.NoError(t, err)
	handleCancelWakeSchedule(t.Context(), a, client, WSMessage{ID: "req", Type: CmdCancelWakeSchedule, Payload: payload})
	return readWakeSchedulesReply(t, client)
}

func TestHandleCancelWakeSchedule_CancelsAndRepliesWithSnapshot(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	sess, err := a.Sessions.Create(t.Context(), "ws cancel")
	require.NoError(t, err)
	now := time.Now()
	id := createSchedule(t, a, sess.ID, "once", "cancel me", now.Add(time.Minute), 0, 0)
	keepID := createSchedule(t, a, sess.ID, "loop", "keep me", now.Add(time.Hour), time.Hour, 0)

	snap, errText, ok := cancelSchedule(t, a, client, sess.ID, id)
	require.True(t, ok, "unexpected error: %q", errText)
	require.Len(t, snap.Schedules, 2)
	byID := map[string]WakeScheduleWire{}
	for _, s := range snap.Schedules {
		byID[s.ID] = s
	}
	require.Equal(t, "cancelled", byID[id].State)
	require.Equal(t, "active", byID[keepID].State)

	// A second cancel of the already-cancelled row is idempotent success.
	_, errText, ok = cancelSchedule(t, a, client, sess.ID, id)
	require.True(t, ok, "second cancel must be idempotent, got %q", errText)
}

func TestHandleCancelWakeSchedule_UnknownIDIsRefused(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	sess, err := a.Sessions.Create(t.Context(), "ws cancel unknown")
	require.NoError(t, err)
	_, errText, ok := cancelSchedule(t, a, client, sess.ID, "wake_does-not-exist")
	require.False(t, ok)
	require.Contains(t, errText, "not found")
}

func TestHandleCancelWakeSchedule_ForeignIDIsRefused(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	mine, err := a.Sessions.Create(t.Context(), "ws cancel mine")
	require.NoError(t, err)
	other, err := a.Sessions.Create(t.Context(), "ws cancel other")
	require.NoError(t, err)
	foreign := createSchedule(t, a, other.ID, "once", "not yours", time.Now().Add(time.Minute), 0, 0)

	_, errText, ok := cancelSchedule(t, a, client, mine.ID, foreign)
	require.False(t, ok)
	require.Contains(t, errText, "not found")

	// The foreign schedule is untouched.
	snap, errText2, ok := requestWakeSchedules(t, a, client, other.ID)
	require.True(t, ok, "unexpected error: %q", errText2)
	require.Len(t, snap.Schedules, 1)
	require.Equal(t, "active", snap.Schedules[0].State)
}

func TestHandleCancelWakeSchedule_UnknownSessionIsRefused(t *testing.T) {
	a, client := newWakeSchedulesTestApp(t)
	_, errText, ok := cancelSchedule(t, a, client, "does-not-exist", "wake_x")
	require.False(t, ok)
	require.NotEmpty(t, errText)
}
