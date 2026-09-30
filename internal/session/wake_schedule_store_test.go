// WakeScheduleStore coverage (stage 4a): claim/lease CAS, exactly-once
// firing, loop advancement from the scheduled time (no catch-up), the
// 20-active limit, idempotent owner-checked cancel, session-delete cascade
// and `sessions reset` cancellation. All time is injected; no sleeps.

package session

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

// wakeFx is the wake-schedule test fixture: a real migrated SQLite DB plus
// a seeded owner session and an injected clock origin.
type wakeFx struct {
	t    *testing.T
	fx   *rerunFx
	wake *WakeScheduleStore
	q    *db.Queries
	ctx  context.Context
	base time.Time
}

func newWakeFx(t *testing.T) *wakeFx {
	t.Helper()
	f := &wakeFx{t: t, fx: newRerunFx(t), ctx: context.Background(), base: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	f.wake = NewWakeScheduleStore(f.fx.store.sqlDB)
	f.q = f.fx.q
	return f
}

func (f *wakeFx) create(kind WakeKind, runAt time.Time, mut func(*CreateWakeScheduleParams)) db.WakeSchedule {
	f.t.Helper()
	p := CreateWakeScheduleParams{Owner: rerunOwner, Kind: kind, Message: "check on it", RunAt: runAt}
	if kind == WakeKindLoop {
		p.Every = 300 * time.Second
	}
	if mut != nil {
		mut(&p)
	}
	row, err := f.wake.CreateSchedule(f.ctx, p, f.base)
	require.NoError(f.t, err)
	return row
}

func (f *wakeFx) get(id string) (db.WakeSchedule, error) {
	return f.wake.q.GetWakeSchedule(f.ctx, id)
}

func (f *wakeFx) noticeCount() int {
	rows, err := f.fx.q.ListSessionNoticesForOwner(f.ctx, rerunOwner)
	require.NoError(f.t, err)
	n := 0
	for _, r := range rows {
		if r.Kind == NoticeKindWakeFired {
			n++
		}
	}
	return n
}

func TestWakeScheduleStore_CreateValidation(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	later := f.base.Add(time.Minute)
	cases := []struct {
		name string
		p    CreateWakeScheduleParams
	}{
		{"no owner", CreateWakeScheduleParams{Kind: WakeKindOnce, Message: "m", RunAt: later}},
		{"empty message", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKindOnce, RunAt: later}},
		{"long message", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKindOnce, Message: string(make([]rune, MaxWakeMessageLength+1)), RunAt: later}},
		{"once too soon", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKindOnce, Message: "m", RunAt: f.base.Add(time.Second)}},
		{"once in the past", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKindOnce, Message: "m", RunAt: f.base.Add(-time.Minute)}},
		{"once over horizon", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKindOnce, Message: "m", RunAt: f.base.Add(MaxWakeHorizon + time.Hour)}},
		{"loop interval too tight", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKindLoop, Message: "m", RunAt: later, Every: time.Minute}},
		{"loop max_runs too big", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKindLoop, Message: "m", RunAt: later, Every: 300 * time.Second, MaxRuns: MaxLoopRuns + 1}},
		{"loop until in the past", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKindLoop, Message: "m", RunAt: later, Every: 300 * time.Second, Until: &f.base}},
		{"unknown kind", CreateWakeScheduleParams{Owner: rerunOwner, Kind: WakeKind("cron"), Message: "m", RunAt: later}},
	}
	for _, tc := range cases {
		_, err := f.wake.CreateSchedule(f.ctx, tc.p, f.base)
		require.Error(f.t, err, tc.name)
	}
}

// SCHED-4: at most 20 active schedules per session; a cancel frees a slot.
func TestWakeScheduleStore_LimitTwentyActive(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	var firstID string
	for i := 0; i < MaxActiveWakeSchedulesPerSession; i++ {
		row := f.create(WakeKindOnce, f.base.Add(time.Duration(i+1)*time.Minute), nil)
		if i == 0 {
			firstID = row.ID
		}
	}
	_, err := f.wake.CreateSchedule(f.ctx, CreateWakeScheduleParams{
		Owner: rerunOwner, Kind: WakeKindOnce, Message: "m", RunAt: f.base.Add(time.Hour),
	}, f.base)
	require.ErrorIs(t, err, ErrWakeScheduleLimit)

	require.NoError(t, f.wake.CancelSchedule(f.ctx, rerunOwner, firstID, f.base))
	_, err = f.wake.CreateSchedule(f.ctx, CreateWakeScheduleParams{
		Owner: rerunOwner, Kind: WakeKindOnce, Message: "m", RunAt: f.base.Add(time.Hour),
	}, f.base)
	require.NoError(t, err, "a cancelled schedule frees a slot")
}

// SCHED-1: once fires exactly one occurrence and is done; a replayed fire
// with the same lease (or after done) changes nothing.
//
// REVERT CHECK: removing the `state='active'` guard from
// CompleteOnceWakeSchedule (or firing outside the CAS) lets the replayed
// FireOccurrence insert a second wake_fired notice; the count assertion
// below fails.
func TestWakeScheduleStore_OnceFiresExactlyOnce(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	due := f.base.Add(time.Minute)
	row := f.create(WakeKindOnce, due, nil)
	claimed, err := f.wake.ClaimDue(f.ctx, "worker-1", due, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	fire, err := f.wake.FireOccurrence(f.ctx, row.ID, "worker-1", due)
	require.NoError(t, err)
	require.True(t, fire.Fired)
	require.True(t, fire.Done)

	again, err := f.wake.FireOccurrence(f.ctx, row.ID, "worker-1", due)
	require.NoError(t, err)
	require.False(t, again.Fired, "the same lease must never fire twice")

	got, err := f.get(row.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.State)
	require.EqualValues(t, 1, f.noticeCount())
}

// SCHED-2: a live lease excludes a second claimant; an expired lease frees
// the row for another claimant, and recovery clears stale leases.
func TestWakeScheduleStore_LeaseExpiryHandsRowToAnotherClaimant(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	due := f.base.Add(time.Minute)
	f.create(WakeKindOnce, due, nil)

	claimed, err := f.wake.ClaimDue(f.ctx, "worker-1", due, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	again, err := f.wake.ClaimDue(f.ctx, "worker-2", due, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Empty(t, again, "a live lease must exclude another claimant")

	n, err := f.wake.RecoverExpiredLeases(f.ctx, due.Add(WakeLeaseTTL-time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 0, n, "the lease has not expired yet")

	n, err = f.wake.RecoverExpiredLeases(f.ctx, due.Add(WakeLeaseTTL+time.Second))
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	taken, err := f.wake.ClaimDue(f.ctx, "worker-2", due.Add(WakeLeaseTTL+time.Second), 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, taken, 1, "the expired lease's row goes to the next claimant")
	require.Equal(t, "worker-2", taken[0].LeaseOwner.String)
}

// SCHED-1/SCHED-2 under real concurrency: N schedulers claim and fire the
// same due row at once; exactly one occurrence is produced. SQLite's single
// writer connection serializes the writes, so each CAS has a single winner.
func TestWakeScheduleStore_ConcurrentClaimAndFireIsExactlyOnce(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	due := f.base.Add(time.Minute)
	row := f.create(WakeKindLoop, due, nil)

	const workers = 8
	var wg sync.WaitGroup
	var fired atomic.Int64
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := "worker-" + strconv.Itoa(i)
			claimed, err := f.wake.ClaimDue(f.ctx, owner, due, 10, WakeLeaseTTL)
			if err != nil {
				return
			}
			for _, c := range claimed {
				fire, err := f.wake.FireOccurrence(f.ctx, c.ID, owner, due)
				if err == nil && fire.Fired {
					fired.Add(1)
				}
			}
		}(i)
	}
	wg.Wait()

	require.EqualValues(t, 1, fired.Load(), "exactly one worker wins the occurrence")
	require.EqualValues(t, 1, f.noticeCount())
	got, err := f.get(row.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, got.Occurrence)
}

// Loop cadence is computed from the SCHEDULED time: t0, t0+300, t0+600;
// max_runs=3 finishes the schedule after the third fire.
func TestWakeScheduleStore_LoopOrderAndMaxRuns(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	start := f.base.Add(time.Minute)
	row := f.create(WakeKindLoop, start, func(p *CreateWakeScheduleParams) { p.MaxRuns = 3 })

	now := start
	for i := 1; i <= 3; i++ {
		claimed, err := f.wake.ClaimDue(f.ctx, "worker-1", now, 10, WakeLeaseTTL)
		require.NoError(t, err)
		require.Len(t, claimed, 1, "fire %d", i)
		fire, err := f.wake.FireOccurrence(f.ctx, row.ID, "worker-1", now)
		require.NoError(t, err)
		require.True(t, fire.Fired)
		if i < 3 {
			require.False(t, fire.Done)
			require.Equal(t, start.Add(time.Duration(i)*300*time.Second).Unix(), fire.NextRun.Unix())
		} else {
			require.True(t, fire.Done, "max_runs=3 finishes after the third fire")
			require.Nil(t, fire.NextRun)
		}
		now = now.Add(300 * time.Second)
	}
	got, err := f.get(row.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.State)
	require.EqualValues(t, 2, got.Occurrence, "the finishing third fire does not advance the counter")
	require.EqualValues(t, 3, f.noticeCount())
}

// until_at: when the NEXT boundary would cross until, the loop finishes
// after firing the in-horizon occurrence.
func TestWakeScheduleStore_LoopUntilFinishes(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	start := f.base.Add(time.Minute)
	until := start.Add(400 * time.Second) // after fire 1, before boundary 2
	row := f.create(WakeKindLoop, start, func(p *CreateWakeScheduleParams) { p.Until = &until })

	// Fire 1 advances (next=start+300 is still inside until=start+400);
	// fire 2 finishes because the NEXT boundary (start+600) crosses until.
	claimed, err := f.wake.ClaimDue(f.ctx, "worker-1", start, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	fire, err := f.wake.FireOccurrence(f.ctx, row.ID, "worker-1", start)
	require.NoError(t, err)
	require.True(t, fire.Fired)
	require.False(t, fire.Done, "the next boundary (start+300) is inside until (start+400)")

	claimed, err = f.wake.ClaimDue(f.ctx, "worker-1", *fire.NextRun, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	fire, err = f.wake.FireOccurrence(f.ctx, row.ID, "worker-1", *fire.NextRun)
	require.NoError(t, err)
	require.True(t, fire.Fired)
	require.True(t, fire.Done, "the next boundary (start+600) crosses until (start+400)")

	got, err := f.get(row.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.State)
}

// SCHED-3: a long downtime skips the missed intervals -- exactly one
// immediate firing, and the next deadline is the first grid boundary after
// now, computed from the scheduled time.
func TestWakeScheduleStore_LoopSkipsMissedIntervals(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	start := f.base.Add(time.Minute)
	row := f.create(WakeKindLoop, start, nil)

	after := start.Add(30000 * time.Second) // 100 intervals missed
	claimed, err := f.wake.ClaimDue(f.ctx, "worker-1", after, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	fire, err := f.wake.FireOccurrence(f.ctx, row.ID, "worker-1", after)
	require.NoError(t, err)
	require.True(t, fire.Fired)
	require.False(t, fire.Done)
	// First grid boundary strictly after `after`, counted from `start`.
	skipped := int64(30000/300) + 1
	require.Equal(t, start.Add(time.Duration(skipped)*300*time.Second).Unix(), fire.NextRun.Unix())

	nothing, err := f.wake.ClaimDue(f.ctx, "worker-1", after, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Empty(t, nothing, "no catch-up: one firing per downtime, then the grid")

	next, err := f.wake.NextDue(f.ctx)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, fire.NextRun.Unix(), next.Unix())
}

// SCHED-5: cancel is idempotent for the owner, refused for a foreign
// session, and never resurrects a cancelled schedule.
func TestWakeScheduleStore_CancelIdempotentAndOwned(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	due := f.base.Add(time.Minute)
	row := f.create(WakeKindOnce, due, nil)

	require.NoError(t, f.wake.CancelSchedule(f.ctx, rerunOwner, row.ID, f.base))
	require.NoError(t, f.wake.CancelSchedule(f.ctx, rerunOwner, row.ID, f.base), "second cancel is a no-op")
	require.ErrorIs(t, f.wake.CancelSchedule(f.ctx, "other-session", row.ID, f.base), ErrWakeScheduleNotOwned)

	claimed, err := f.wake.ClaimDue(f.ctx, "worker-1", due, 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Empty(t, claimed, "a cancelled schedule is never claimed")
	fire, err := f.wake.FireOccurrence(f.ctx, row.ID, "worker-1", due)
	require.NoError(t, err)
	require.False(t, fire.Fired)

	got, err := f.get(row.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", got.State)
	require.EqualValues(t, 0, f.noticeCount())
}

// Cancel of an unknown id is a no-op, and ClaimDue honours its limit.
func TestWakeScheduleStore_CancelUnknownAndClaimLimit(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	require.NoError(t, f.wake.CancelSchedule(f.ctx, rerunOwner, "wake_missing", f.base))

	f.create(WakeKindOnce, f.base.Add(time.Minute), nil)
	f.create(WakeKindOnce, f.base.Add(2*time.Minute), nil)
	claimed, err := f.wake.ClaimDue(f.ctx, "worker-1", f.base.Add(time.Hour), 1, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1, "the claim honours its limit")
	claimed, err = f.wake.ClaimDue(f.ctx, "worker-2", f.base.Add(time.Hour), 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1, "the second claimant gets the disjoint remainder")
}

// The owner FK cascades: deleting the session deletes its schedules.
func TestWakeScheduleStore_SessionDeleteCascades(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	row := f.create(WakeKindLoop, f.base.Add(time.Minute), nil)

	require.NoError(t, f.q.DeleteSession(f.ctx, rerunOwner))
	_, err := f.get(row.ID)
	require.ErrorIs(t, err, sql.ErrNoRows, "the schedule is gone with its session")
}

// `sessions reset` cancels every ACTIVE schedule of the owner in the reset
// transaction; done/cancelled history stays, like the voided ledger rows.
func TestWakeScheduleStore_ResetCancelsActive(t *testing.T) {
	t.Parallel()
	f := newWakeFx(t)
	active := f.create(WakeKindLoop, f.base.Add(10*time.Minute), nil)
	done := f.create(WakeKindOnce, f.base.Add(2*time.Minute), nil)
	fire, err := f.wake.FireOccurrence(f.ctx, done.ID, "", f.base.Add(2*time.Minute))
	require.NoError(t, err)
	require.False(t, fire.Fired, "firing without a lease is a no-op; finish the row via claim+fire")

	claimed, err := f.wake.ClaimDue(f.ctx, "worker-1", f.base.Add(2*time.Minute), 10, WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	_, err = f.wake.FireOccurrence(f.ctx, done.ID, "worker-1", f.base.Add(2*time.Minute))
	require.NoError(t, err)

	out, err := f.fx.store.ResetOwnerHistory(f.ctx, f.fx.messages, rerunOwner)
	require.NoError(t, err)
	require.EqualValues(t, 1, out.SchedulesCancelled, "only the active one")

	got, err := f.get(active.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", got.State)
	got, err = f.get(done.ID)
	require.NoError(t, err)
	require.Equal(t, "done", got.State, "history is not resurrected or deleted by a reset")
}

// The migration re-runs cleanly on a DB that already has data (goose is
// idempotent), and the schema survives the reopen.
func TestWakeScheduleMigration_IdempotentOnExistingData(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	ctx := context.Background()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	q := db.New(conn)
	require.NoError(t, seedSession(ctx, q, "wake-mig"))
	require.NoError(t, conn.Close())
	require.NoError(t, db.Release(dataDir))

	conn, err = db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	q = db.New(conn)
	var count int
	require.NoError(t, conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM wake_schedules`).Scan(&count))
	row, err := q.GetSessionByID(ctx, "wake-mig")
	require.NoError(t, err)
	require.Equal(t, "wake-mig", row.ID)
}
