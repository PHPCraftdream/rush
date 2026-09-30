// Stage-4b wake-scheduler tests (wake_scheduler.go). All time is injected
// (the scheduler's now/after fields): no sleeps for synchronization — the
// test advances the clock and fires the timer channel itself, and the
// scheduler's after callback reports each re-arm on a channel so every
// step is a deterministic handoff. Revert-checks are noted per test.

package agent

import (
	"context"
	"database/sql"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// schedFx is the deterministic wake-scheduler fixture: a migrated SQLite
// store, a manual clock, a manual timer channel, and a recorded wake
// delivery.
type schedFx struct {
	t     *testing.T
	store *session.WakeScheduleStore
	q     *db.Queries
	conn  *sql.DB
	ctx   context.Context

	clock atomic.Int64 // UnixNano; the scheduler's now()

	mu     sync.Mutex
	timerC chan time.Time     // the channel the run loop is currently parked on
	waits  []time.Duration    // every duration after() was called with
	armed  chan time.Duration // one send per after() call
	woken  chan string        // one send per delivered wake
}

func newSchedFx(t *testing.T) *schedFx {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(dataDir)) })
	_, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`)
	require.NoError(t, err)

	fx := &schedFx{
		t:      t,
		store:  session.NewWakeScheduleStore(conn),
		q:      db.New(conn),
		conn:   conn,
		ctx:    ctx,
		timerC: make(chan time.Time, 1),
		armed:  make(chan time.Duration, 32),
		woken:  make(chan string, 32),
	}
	fx.setBase(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	return fx
}

func (fx *schedFx) setBase(base time.Time) {
	fx.clock.Store(base.UnixNano())
}

func (fx *schedFx) now() time.Time { return time.Unix(0, fx.clock.Load()) }

// start builds and starts a scheduler over fx.store, waiting for its
// startup lease recovery to complete and for the first timer arm.
func (fx *schedFx) start(leaseOwner string) *wakeScheduler {
	s := newWakeScheduler(fx.store, leaseOwner, func(_ context.Context, sessionID string) {
		fx.woken <- sessionID
	})
	s.now = fx.now
	s.after = fx.after
	s.Start()
	fx.t.Cleanup(s.Stop)
	<-s.started
	<-fx.armed // the initial arm
	return s
}

func (fx *schedFx) after(d time.Duration) <-chan time.Time {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.waits = append(fx.waits, d)
	fx.timerC = make(chan time.Time, 1)
	fx.armed <- d
	return fx.timerC
}

// lastWait returns the most recent after() duration.
func (fx *schedFx) lastWait() time.Duration {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	require.NotEmpty(fx.t, fx.waits)
	return fx.waits[len(fx.waits)-1]
}

// reschedule arms the scheduler around sessionID's current nearest due and
// returns the wait it chose.
func (fx *schedFx) reschedule(s *wakeScheduler) time.Duration {
	s.Reschedule()
	return <-fx.armed
}

// fire advances the clock by d and releases the current timer.
func (fx *schedFx) fire(d time.Duration) {
	fx.clock.Store(fx.now().Add(d).UnixNano())
	fx.mu.Lock()
	c := fx.timerC
	fx.mu.Unlock()
	require.NotNil(fx.t, c)
	c <- time.Now()
}

func (fx *schedFx) createOnce(owner string, delay time.Duration) session.CreateWakeScheduleParams {
	return session.CreateWakeScheduleParams{Owner: owner, Kind: session.WakeKindOnce, Message: "check it", RunAt: fx.now().Add(delay)}
}

// insertLoop writes a loop schedule directly, bypassing CreateSchedule's
// ≥300s validation: the worker reads whatever next_run_at/every_ms say, and
// sub-minute intervals keep every timer wait under the 60s recompute cap
// so the deterministic harness can target it exactly.
func (fx *schedFx) insertLoop(owner string, runAt, every time.Duration, maxRuns int, until *time.Time) db.WakeSchedule {
	var maxRunsN, untilN sql.NullInt64
	if maxRuns > 0 {
		maxRunsN = sql.NullInt64{Int64: int64(maxRuns), Valid: true}
	}
	if until != nil {
		untilN = sql.NullInt64{Int64: until.Unix(), Valid: true}
	}
	now := fx.now().Unix()
	row, err := fx.q.InsertWakeSchedule(fx.ctx, db.InsertWakeScheduleParams{
		ID:             "wake_test_loop_" + strconv.FormatInt(now, 10) + "_" + owner,
		OwnerSessionID: owner, Kind: "loop", Message: "tick",
		NextRunAt: fx.now().Add(runAt).Unix(), EveryMs: every.Milliseconds(),
		MaxRuns: maxRunsN, UntilAt: untilN, CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(fx.t, err)
	return row
}

func (fx *schedFx) noticeCount(owner string) int {
	rows, err := fx.q.ListSessionNoticesForOwner(fx.ctx, owner)
	require.NoError(fx.t, err)
	n := 0
	for _, r := range rows {
		if r.Kind == session.NoticeKindWakeFired {
			n++
		}
	}
	return n
}

func (fx *schedFx) schedule(id string) db.WakeSchedule {
	row, err := fx.q.GetWakeSchedule(fx.ctx, id)
	require.NoError(fx.t, err)
	return row
}

// TestWakeScheduler_OnceFiresOneWakeOneNotice: a due once schedule produces
// exactly one wake and one wake_fired notice, marks the row done, and a
// later tick does not fire it again.
//
// Revert-check performed: made fireDue skip FireOccurrence (continue
// before the call) — this test FAILED (deadlock waiting for the wake that
// never came). Restored; re-ran, passed.
func TestWakeScheduler_OnceFiresOneWakeOneNotice(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("w1")

	row, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-1", 10*time.Second), fx.now())
	require.NoError(t, err)
	wait := fx.reschedule(s)
	require.Equal(t, 10*time.Second, wait, "the timer must target the schedule's due time")

	fx.fire(10 * time.Second)
	require.Equal(t, "sess-1", <-fx.woken)
	require.Equal(t, 1, fx.noticeCount("sess-1"))
	done := fx.schedule(row.ID)
	require.Equal(t, "done", done.State)

	// The recompute after the fire sees no schedule left: idle-cap wait.
	idle := <-fx.armed
	require.Equal(t, wakeSchedulerMaxWait, idle)

	// A second due tick must not re-fire.
	fx.fire(wakeSchedulerMaxWait)
	<-fx.armed
	require.Equal(t, 1, fx.noticeCount("sess-1"))
	select {
	case id := <-fx.woken:
		t.Fatalf("unexpected second wake for %s", id)
	default:
	}
}

// TestWakeScheduler_LoopMaxRunsAndGrid: loop fires in order on its grid,
// stops at max_runs, and a later tick is inert. The max_runs branch itself
// is store-level (FinishLoopWakeSchedule, SCHED-1; proven by
// TestWakeScheduleStore_LoopOrderAndMaxRuns in internal/session); this
// test pins the worker's end-to-end ordering on top of it.
func TestWakeScheduler_LoopMaxRunsAndGrid(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("w1")

	row := fx.insertLoop("sess-1", 10*time.Second, 10*time.Second, 2, nil)
	require.Equal(t, 10*time.Second, fx.reschedule(s))

	for i := 0; i < 2; i++ {
		// After a fire the loop re-arms by itself; the armed read is the
		// sync point (a Reschedule here could fire a stale timer channel).
		if i > 0 {
			require.Equal(t, 10*time.Second, <-fx.armed)
		}
		fx.fire(10 * time.Second)
		require.Equal(t, "sess-1", <-fx.woken)
	}
	done := fx.schedule(row.ID)
	require.Equal(t, "done", done.State)
	// FinishLoopWakeSchedule (4a) leaves `occurrence` at the last ADVANCED
	// value; the final (finishing) fire is counted by its notice, not the
	// column.
	require.Equal(t, int64(1), done.Occurrence)
	require.Equal(t, 2, fx.noticeCount("sess-1"))

	require.Equal(t, wakeSchedulerMaxWait, <-fx.armed) // idle recompute
	fx.fire(wakeSchedulerMaxWait)
	require.Equal(t, 2, fx.noticeCount("sess-1"))
}

// TestWakeScheduler_LoopUntilStops: a loop whose next grid boundary would
// cross until finishes after the last in-bounds occurrence.
func TestWakeScheduler_LoopUntilStops(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("w1")

	until := fx.now().Add(25 * time.Second)
	row := fx.insertLoop("sess-1", 10*time.Second, 10*time.Second, 0, &until)
	require.Equal(t, 10*time.Second, fx.reschedule(s))

	for i := 0; i < 2; i++ {
		if i > 0 {
			require.Equal(t, 10*time.Second, <-fx.armed)
		}
		fx.fire(10 * time.Second)
		<-fx.woken
	}
	done := fx.schedule(row.ID)
	require.Equal(t, "done", done.State)
	require.Equal(t, 2, fx.noticeCount("sess-1"))
}

// TestWakeScheduler_RestartRecoversWithoutDuplicates: a lease left claimed
// by a dead scheduler is recovered by a fresh worker on the same DB, and
// the occurrence fires exactly once across the restart.
func TestWakeScheduler_RestartRecoversWithoutDuplicates(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("dead-worker")

	// Simulate the dead worker: it claimed the due occurrence and died
	// before firing (its lease outlives it until TTL expiry).
	_, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-1", 10*time.Second), fx.now())
	require.NoError(t, err)
	fx.clock.Store(fx.now().Add(20 * time.Second).UnixNano())
	claimed, err := fx.store.ClaimDue(fx.ctx, "dead-worker", fx.now(), 10, session.WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	s.Stop() // the "crash": its lease is now orphaned

	// Restart well past the lease TTL: recovery must hand the row over.
	fx.clock.Store(fx.now().Add(2 * session.WakeLeaseTTL).UnixNano())
	fx.start("fresh-worker")
	require.False(t, fx.schedule(claimed[0].ID).LeaseOwner.Valid,
		"startup recovery must have cleared the dead worker's lease")
	// The row is due NOW: the restart's first arm already targeted 0.
	require.Equal(t, time.Duration(0), fx.lastWait())
	fx.fire(0)
	require.Equal(t, "sess-1", <-fx.woken)
	require.Equal(t, 1, fx.noticeCount("sess-1"))
	require.Equal(t, "done", fx.schedule(claimed[0].ID).State)
}

// TestWakeScheduler_OverdueAfterDowntimeFiresOnceNoCatchUp: a loop overdue
// by many intervals after downtime fires ONE immediate occurrence and
// lands back on its original grid. The no-catch-up arithmetic lives in
// nextLoopRun (internal/session/wake_schedule_fire.go, proven by
// TestWakeScheduleStore_LoopSkipsMissedIntervals); the grid assertion here
// is the worker-level guard of SCHED-3.
func TestWakeScheduler_OverdueAfterDowntimeFiresOnceNoCatchUp(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("w1")

	start := fx.now()
	row, err := fx.store.CreateSchedule(fx.ctx, session.CreateWakeScheduleParams{
		Owner: "sess-1", Kind: session.WakeKindLoop, Message: "tick",
		RunAt: start.Add(300 * time.Second), Every: 300 * time.Second,
	}, start)
	require.NoError(t, err)

	// Long downtime past 10 intervals, then the worker's timer (already
	// armed around the old due by the reschedule below) releases.
	fx.reschedule(s)
	fx.fire(10 * 300 * time.Second)
	require.Equal(t, "sess-1", <-fx.woken)
	require.Equal(t, 1, fx.noticeCount("sess-1"), "exactly one immediate occurrence, no catch-up")

	fresh := fx.schedule(row.ID)
	next := time.Unix(fresh.NextRunAt, 0)
	require.False(t, next.Before(fx.now()), "next_run_at must be in the future")
	require.Zero(t, (next.Sub(start))%(300*time.Second), "next_run_at must stay on the original grid")
}

// TestWakeScheduler_TwoSchedulersOneOccurrence: two workers on the same DB
// racing the same due occurrence produce exactly one wake. The lease-CAS
// proof is store-level (TestWakeScheduleStore_ConcurrentClaimAndFire-
// IsExactlyOnce); this test pins the worker-level consequence.
func TestWakeScheduler_TwoSchedulersOneOccurrence(t *testing.T) {
	fx := newSchedFx(t)
	// CreateSchedule's own validation demands a ≥5s horizon; due-ness comes
	// from advancing the clock before the racing fireDue calls.
	_, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-1", 5*time.Second), fx.now())
	require.NoError(t, err)
	fx.clock.Store(fx.now().Add(10 * time.Second).UnixNano())

	var wg sync.WaitGroup
	wg.Add(2)
	for _, owner := range []string{"w-a", "w-b"} {
		go func(owner string) {
			defer wg.Done()
			s := newWakeScheduler(fx.store, owner, func(_ context.Context, id string) { fx.woken <- id })
			s.now = fx.now
			s.after = func(time.Duration) <-chan time.Time { c := make(chan time.Time, 1); close(c); return c }
			s.fireDue()
		}(owner)
	}
	wg.Wait()

	select {
	case <-fx.woken:
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case id := <-fx.woken:
		t.Fatalf("second wake from %q", id)
	default:
	}
	require.Equal(t, 1, fx.noticeCount("sess-1"))
}

// TestWakeScheduler_CancelWhileWaiting: cancelling the schedule while the
// worker's timer is armed must never fire. The state-level refusal is
// SCHED-5's store-level proof (TestWakeScheduleStore_CancelIdempotentAnd-
// Owned); this pins the end-to-end no-fire.
func TestWakeScheduler_CancelWhileWaiting(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("w1")

	row, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-1", 10*time.Second), fx.now())
	require.NoError(t, err)
	fx.reschedule(s)

	require.NoError(t, fx.store.CancelSchedule(fx.ctx, "sess-1", row.ID, fx.now()))
	fx.fire(10 * time.Second)
	<-fx.armed // the post-tick recompute: fireDue has run to completion
	require.Equal(t, 0, fx.noticeCount("sess-1"))
	select {
	case id := <-fx.woken:
		t.Fatalf("fired a cancelled schedule, woke %s", id)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestWakeScheduler_DeliveryErrorKeepsOccurrence: a wake delivery that
// goes nowhere must not lose or duplicate the occurrence — the durable
// notice stays pending for the owner's next pull, and no later tick
// re-fires the (already terminal) schedule.
func TestWakeScheduler_DeliveryErrorKeepsOccurrence(t *testing.T) {
	fx := newSchedFx(t)
	// No woken reader wired: the "delivery" discards the event (a failed
	// wakeSession), like supervision's `_ = coord.wakeSession(...)`.
	s := newWakeScheduler(fx.store, "w1", func(context.Context, string) {})
	s.now = fx.now
	s.after = fx.after
	s.Start()
	fx.t.Cleanup(s.Stop)
	<-s.started
	<-fx.armed

	_, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-1", 10*time.Second), fx.now())
	require.NoError(t, err)
	fx.reschedule(s)
	fx.fire(10 * time.Second)
	// The post-fire recompute is the sync point: fireDue has fully
	// committed by the time the loop re-arms (the wake callback here is a
	// no-op, so it provides no synchronization of its own).
	<-fx.armed

	require.Equal(t, 1, fx.noticeCount("sess-1"), "the durable notice must exist despite the failed delivery")
	fx.fire(wakeSchedulerMaxWait)
	<-fx.armed
	require.Equal(t, 1, fx.noticeCount("sess-1"), "no re-fire after a failed delivery")
}

// TestWakeScheduler_DeadOwnerNoLoop: a schedule whose owner session was
// deleted mid-lease (cascade) must not wake anyone, must not error, and
// must not loop on the row.
func TestWakeScheduler_DeadOwnerNoLoop(t *testing.T) {
	fx := newSchedFx(t)
	fx.start("w1")

	row, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-gone", 10*time.Second), fx.now())
	require.NoError(t, err)
	// Make it due, then simulate the race: another claimant holds the lease
	// while the owner session disappears (the schedule row cascades away).
	fx.clock.Store(fx.now().Add(10 * time.Second).UnixNano())
	claimed, err := fx.store.ClaimDue(fx.ctx, "other", fx.now(), 10, session.WakeLeaseTTL)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	_, err = fx.conn.ExecContext(fx.ctx, `DELETE FROM wake_schedules WHERE id = ?`, row.ID)
	require.NoError(t, err)

	fx.fire(0)
	<-fx.armed // the post-tick recompute: fireDue has run to completion
	require.Equal(t, 0, fx.noticeCount("sess-gone"))
	select {
	case id := <-fx.woken:
		t.Fatalf("woke a deleted session: %s", id)
	default:
	}
}

// TestWakeScheduler_StopExitsGoroutine: Stop terminates the worker
// goroutine (no leak), and is idempotent + nil-safe.
func TestWakeScheduler_StopExitsGoroutine(t *testing.T) {
	fx := newSchedFx(t)
	s := fx.start("w1")
	s.Stop()
	s.Stop() // idempotent
	var nilS *wakeScheduler
	nilS.Stop() // nil-safe
	goleak.VerifyNone(t, goleak.IgnoreCurrent())
}

// TestCoordinatorHasOpenOnceWakeSchedule: the stage-5a predicate — an
// active once schedule counts, a loop does not, a cancelled once does not,
// and a coordinator without a scheduler is a clean false.
func TestCoordinatorHasOpenOnceWakeSchedule(t *testing.T) {
	fx := newSchedFx(t)
	c := &coordinator{wakeScheduler: &wakeScheduler{store: fx.store}}
	once, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-1", 10*time.Second), fx.now())
	require.NoError(t, err)
	ok, err := c.HasOpenOnceWakeSchedule(fx.ctx, "sess-1")
	require.NoError(t, err)
	require.True(t, ok, "an active once schedule must hold the run open")

	require.NoError(t, fx.store.CancelSchedule(fx.ctx, "sess-1", once.ID, fx.now()))
	ok, err = c.HasOpenOnceWakeSchedule(fx.ctx, "sess-1")
	require.NoError(t, err)
	require.False(t, ok, "a cancelled once schedule holds nothing")

	_, err = fx.store.CreateSchedule(fx.ctx, session.CreateWakeScheduleParams{
		Owner: "sess-1", Kind: session.WakeKindLoop, Message: "tick",
		RunAt: fx.now().Add(300 * time.Second), Every: 300 * time.Second,
	}, fx.now())
	require.NoError(t, err)
	ok, err = c.HasOpenOnceWakeSchedule(fx.ctx, "sess-1")
	require.NoError(t, err)
	require.False(t, ok, "a loop never holds the run open (operator decision)")
}

// TestWakeFiredNoticeIsNotAHumanMessage: the wake_fired notice must reach
// the provider history as a system event, not a human turn — the same
// BackgroundJobNotice/AutoResumed marking every other wake=1 session
// notice uses, and not collapsed by the supervision history filter.
func TestWakeFiredNoticeIsNotAHumanMessage(t *testing.T) {
	params := buildSessionNoticeMessageParams(session.SessionNoticeRow{
		ID: 1, Kind: session.NoticeKindWakeFired, Text: "Wake wake_x fired (scheduled for ...): check it",
	})
	require.True(t, params.BackgroundJobNotice, "must render as a notice, not a human message")
	require.True(t, params.AutoResumed, "a wake_fired event starts the owner's turn itself")
	require.Equal(t, session.NoticeKindWakeFired, params.NoticeKind)
	require.Equal(t, message.User, params.Role)

	// The supervision filter must not collapse wake_fired rows.
	msgs := []message.Message{
		{NoticeKind: session.NoticeKindWakeFired, Parts: []message.ContentPart{message.TextContent{Text: "wake one"}}},
		{NoticeKind: "supervision"},
		{NoticeKind: session.NoticeKindWakeFired, Parts: []message.ContentPart{message.TextContent{Text: "wake two"}}},
	}
	kept := dropSupersededSupervisionNotices(msgs)
	require.Equal(t, "wake one", mustText(t, kept[0]), "wake_fired history must never be collapsed")
	require.Equal(t, "wake two", mustText(t, kept[2]))
}

func mustText(t *testing.T, m message.Message) string {
	t.Helper()
	require.Len(t, m.Parts, 1)
	tc, ok := m.Parts[0].(message.TextContent)
	require.True(t, ok)
	return tc.Text
}
