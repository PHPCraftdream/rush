// Stage-4c coordinator adapter (coordinator_wake_tools.go): contract §1
// validation boundaries, §1.5's single not-found cancel, the 20-active
// limit, Notify re-arming the worker's timer, and the honest no-scheduler
// error. Time is the schedFx injected clock (wake_scheduler_test.go) — no
// sleeps.
package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func wakeCoord(fx *schedFx) *coordinator {
	return &coordinator{wakeScheduler: fx.start("w4c-test")}
}

func TestCoordinatorWakeTools_NoSchedulerHonestError(t *testing.T) {
	t.Parallel()
	c := &coordinator{}
	ctx := t.Context()
	mr := int64(10)
	until := "2030-01-01T00:00:00Z"
	_, err := c.ScheduleWakeOnceDelay(ctx, "s", 10, "m")
	require.ErrorContains(t, err, "wake scheduler is unavailable")
	_, err = c.ScheduleWakeOnceAt(ctx, "s", "2030-01-01T00:00:00Z", "m")
	require.ErrorContains(t, err, "wake scheduler is unavailable")
	_, err = c.ScheduleWakeLoop(ctx, "s", 300, "m", &mr, &until)
	require.ErrorContains(t, err, "wake scheduler is unavailable")
	_, err = c.ListWakeSchedules(ctx, "s")
	require.ErrorContains(t, err, "wake scheduler is unavailable")
	_, err = c.CancelWakeSchedule(ctx, "s", "wake_x")
	require.ErrorContains(t, err, "wake scheduler is unavailable")
}

func TestCoordinatorWakeTools_ValidationBoundaries(t *testing.T) {
	t.Parallel()
	fx := newSchedFx(t)
	c := wakeCoord(fx)
	ctx := t.Context()

	// wakein (§10: 4 → error, 5 → ok; 0/negative → positive; > 100y).
	for _, bad := range []int64{0, -5, 4} {
		_, err := c.ScheduleWakeOnceDelay(ctx, "s", bad, "m")
		require.Error(t, err, "delay_seconds=%d", bad)
	}
	_, err := c.ScheduleWakeOnceDelay(ctx, "s", 5, "m")
	require.NoError(t, err)
	_, err = c.ScheduleWakeOnceDelay(ctx, "s", 3155760000+1, "m")
	require.ErrorContains(t, err, "implausibly large")

	// message (§6): empty and >4000 runes.
	_, err = c.ScheduleWakeOnceDelay(ctx, "s", 10, "")
	require.ErrorContains(t, err, "message is required")
	_, err = c.ScheduleWakeOnceDelay(ctx, "s", 10, string(make([]rune, 4001)))
	require.ErrorContains(t, err, "message exceeds 4000 characters (4001)")
	ok := string(make([]rune, 4000))
	_, err = c.ScheduleWakeOnceDelay(ctx, "s", 10, ok)
	require.NoError(t, err, "exactly 4000 runes is within the §6 limit")

	// wakeon (§10: no zone, past with 2s tolerance, 101 years ahead).
	for _, bad := range []string{
		"2030-01-01T09:00:00",       // naive, no offset
		"2020-01-01T09:00:00+03:00", // in the past relative to the fx clock
		"2127-01-01T09:00:00Z",      // > 100 years
	} {
		_, err := c.ScheduleWakeOnceAt(ctx, "s", bad, "m")
		require.Error(t, err, "at=%q", bad)
	}
	past := fx.now().Add(-3 * time.Second)
	_, err = c.ScheduleWakeOnceAt(ctx, "s", past.Format(time.RFC3339), "m")
	require.ErrorContains(t, err, "is in the past")
	view, err := c.ScheduleWakeOnceAt(ctx, "s", fx.now().Add(time.Minute).Format(time.RFC3339), "m")
	require.NoError(t, err)
	require.Equal(t, fx.now().Add(time.Minute).UTC().Format(time.RFC3339), view.FiresAt,
		"fires_at must be normalized to UTC RFC3339")

	// loop (§10: 299 → error, 300 → ok; max_runs bounds; until rules).
	_, err = c.ScheduleWakeLoop(ctx, "s", 299, "m", nil, nil)
	require.ErrorContains(t, err, "every_seconds must be at least 300")
	_, err = c.ScheduleWakeLoop(ctx, "s", 300, "m", nil, nil)
	require.NoError(t, err)
	_, err = c.ScheduleWakeLoop(ctx, "s", 300, "m", int64Ptr(0), nil)
	require.ErrorContains(t, err, "max_runs must be positive")
	_, err = c.ScheduleWakeLoop(ctx, "s", 300, "m", int64Ptr(10001), nil)
	require.ErrorContains(t, err, "max_runs exceeds the limit (10000)")
	_, err = c.ScheduleWakeLoop(ctx, "s", 300, "m", nil, strPtr("2030-01-01T09:00:00")) // no zone
	require.Error(t, err)
	_, err = c.ScheduleWakeLoop(ctx, "s", 300, "m", nil, strPtr(fx.now().Add(-time.Minute).Format(time.RFC3339)))
	require.ErrorContains(t, err, "is in the past")
	// until earlier than the first scheduled firing.
	_, err = c.ScheduleWakeLoop(ctx, "s", 300, "m", nil, strPtr(fx.now().Add(100*time.Second).Format(time.RFC3339)))
	require.ErrorContains(t, err, "is earlier than the first scheduled firing")
	_, err = c.ScheduleWakeLoop(ctx, "s", 300, "m", nil, strPtr(fx.now().Add(600*time.Second).Format(time.RFC3339)))
	require.NoError(t, err)
}

func int64Ptr(v int64) *int64 { return &v }
func strPtr(v string) *string { return &v }

// SCHED-04 through the tool path: the 21st creation fails with the
// contract's message, a cancel frees the slot.
func TestCoordinatorWakeTools_LimitTwenty(t *testing.T) {
	t.Parallel()
	fx := newSchedFx(t)
	c := wakeCoord(fx)
	ctx := t.Context()

	var first string
	for i := 0; i < 20; i++ {
		view, err := c.ScheduleWakeOnceDelay(ctx, "s", int64(5+i), "m")
		require.NoError(t, err)
		if i == 0 {
			first = view.ScheduleID
		}
	}
	_, err := c.ScheduleWakeOnceDelay(ctx, "s", 100, "m")
	require.ErrorContains(t, err,
		"maximum number of active schedules (20) reached for this session; cancel one with wake_cancel or wait for a one-shot wake to fire")

	_, err = c.CancelWakeSchedule(ctx, "s", first)
	require.NoError(t, err)
	_, err = c.ScheduleWakeOnceDelay(ctx, "s", 100, "m")
	require.NoError(t, err, "a cancelled schedule frees a slot")
}

// §1.5: one and the same not-found error for never-existed, foreign and
// already-cancelled ids; a repeat cancel is a safe no-op with the same
// error; a foreign id is never cancelled.
func TestCoordinatorWakeTools_CancelSingleNotFoundAndOwnership(t *testing.T) {
	t.Parallel()
	fx := newSchedFx(t)
	c := wakeCoord(fx)
	ctx := t.Context()

	view, err := c.ScheduleWakeOnceDelay(ctx, "s", 10, "m")
	require.NoError(t, err)

	// Foreign id: created directly in the store under another owner.
	foreign, err := fx.store.CreateSchedule(ctx, fx.createOnce("other-session", 10*time.Second), fx.now())
	require.NoError(t, err)

	_, err = c.CancelWakeSchedule(ctx, "s", "wake_never-existed")
	require.ErrorContains(t, err, "not found (never existed, already fired, already cancelled, or belongs to another session)")
	_, err = c.CancelWakeSchedule(ctx, "s", foreign.ID)
	require.ErrorContains(t, err, "not found (never existed, already fired, already cancelled, or belongs to another session)",
		"a foreign id gets the same single not-found error")
	require.Equal(t, "active", fx.schedule(foreign.ID).State, "a foreign schedule must never be cancelled")

	_, err = c.CancelWakeSchedule(ctx, "s", view.ScheduleID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", fx.schedule(view.ScheduleID).State)
	// Repeat: same not-found error, not a disguised success.
	_, err = c.CancelWakeSchedule(ctx, "s", view.ScheduleID)
	require.ErrorContains(t, err, "not found (never existed, already fired, already cancelled, or belongs to another session)")
}

// §1.4: wake_list shows only THIS session's active schedules; a fired
// once schedule drops out.
func TestCoordinatorWakeTools_ListOnlyOwnActive(t *testing.T) {
	t.Parallel()
	fx := newSchedFx(t)
	c := wakeCoord(fx)
	ctx := t.Context()

	mine, err := c.ScheduleWakeOnceDelay(ctx, "s", 10, "mine")
	require.NoError(t, err)
	_, err = fx.store.CreateSchedule(ctx, fx.createOnce("other-session", 600*time.Second), fx.now())
	require.NoError(t, err)

	views, err := c.ListWakeSchedules(ctx, "s")
	require.NoError(t, err)
	require.Len(t, views, 1, "another session's schedules never leak into wake_list")
	require.Equal(t, mine.ScheduleID, views[0].ScheduleID)

	// The same handshake EndToEndWakein relies on: wait for the worker's
	// post-Notify re-arm before firing. Without it fire() can release a
	// stale timer channel the run loop has not replaced yet — the worker
	// then parks on the new channel and the fired wake never arrives
	// (lost wakeup, observed as an unbounded hang at the woken read
	// below).
	<-fx.armed

	// Terminal rows are history, not active: fire the only mine schedule.
	fx.fire(10 * time.Second)
	require.Equal(t, "s", <-fx.woken)
	views, err = c.ListWakeSchedules(ctx, "s")
	require.NoError(t, err)
	require.Empty(t, views, "a fired once schedule is no longer active")
}

// End-to-end (task item 6): wakein through the tool-path adapter → the
// worker's timer re-arms via Notify → the due occurrence fires and the
// owner is woken → the durable wake_fired notice carries the schedule_id →
// wake_list no longer lists it.
//
// Revert-check: dropping the adapter's Notify() calls fails the armed-wait
// assertions below (the timer would keep the idle cap instead of the
// schedule's delay).
func TestCoordinatorWakeTools_EndToEndWakein(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("w4c-e2e")
	c := &coordinator{wakeScheduler: s}
	ctx := t.Context()

	view, err := c.ScheduleWakeOnceDelay(ctx, "s", 10, "check the build")
	require.NoError(t, err)
	// Notify re-armed the timer onto this schedule's due time.
	require.Equal(t, 10*time.Second, <-fx.armed)

	fx.fire(10 * time.Second)
	require.Equal(t, "s", <-fx.woken, "the worker delivered the fired occurrence to the owner")

	// The durable wake_fired notice carries the schedule_id (§5.1) and the
	// fired once schedule is no longer active in wake_list.
	require.Equal(t, 1, fx.noticeCount("s"))
	rows, err := fx.q.ListSessionNoticesForOwner(ctx, "s")
	require.NoError(t, err)
	var wakeTexts []string
	for _, r := range rows {
		if r.Kind == "wake_fired" {
			wakeTexts = append(wakeTexts, r.Text)
		}
	}
	require.Len(t, wakeTexts, 1)
	require.Contains(t, wakeTexts[0], view.ScheduleID)
	require.Contains(t, wakeTexts[0], "check the build")
	require.Equal(t, "done", fx.schedule(view.ScheduleID).State)
	views, err := c.ListWakeSchedules(ctx, "s")
	require.NoError(t, err)
	require.Empty(t, views)
}
