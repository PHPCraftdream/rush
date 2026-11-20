package agent

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/wakectl"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/stretchr/testify/require"
)

// schedFx is the deterministic wake-scheduler fixture: a migrated SQLite
// store, a manual clock, a manual timer channel, and a recorded wake
// delivery.
type schedFx struct {
	t     *testing.T
	store *sessionwake.WakeScheduleStore
	q     *db.Queries
	conn  *sql.DB
	ctx   context.Context

	clock atomic.Int64 // UnixNano; the scheduler's now()

	mu     sync.Mutex
	timerC chan time.Time     // the channel the run loop is currently parked on
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
		store:  sessionwake.NewWakeScheduleStore(conn),
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

func (fx *schedFx) scheduler() *wakeScheduleWorker {
	return wakectl.NewScheduler(fx.store, wakectlWakeSink(func(_ context.Context, id string, _ bool) error {
		fx.woken <- id
		return nil
	}), fx.now, fx.after)
}

type wakectlWakeSink func(context.Context, string, bool) error

func (f wakectlWakeSink) WakeSession(ctx context.Context, id string, fact bool) error {
	return f(ctx, id, fact)
}

func (fx *schedFx) start() *wakeScheduleWorker {
	s := fx.scheduler()
	s.Start()
	fx.t.Cleanup(s.Stop)
	<-fx.armed
	return s
}

func (fx *schedFx) after(d time.Duration) <-chan time.Time {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.timerC = make(chan time.Time, 1)
	fx.armed <- d
	return fx.timerC
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

func (fx *schedFx) createOnce(owner string, delay time.Duration) sessionwake.CreateWakeScheduleParams {
	return sessionwake.CreateWakeScheduleParams{Owner: owner, Kind: sessionwake.WakeKindOnce, Message: "check it", RunAt: fx.now().Add(delay)}
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

// TestCoordinatorHasOpenOnceWakeSchedule: the stage-5a predicate — an
// active once schedule counts, a loop does not, a cancelled once does not,
// and a coordinator without a scheduler is a clean false.
func TestCoordinatorHasOpenOnceWakeSchedule(t *testing.T) {
	fx := newSchedFx(t)
	c := &coordinator{wakeScheduler: fx.scheduler()}
	once, err := fx.store.CreateSchedule(fx.ctx, fx.createOnce("sess-1", 10*time.Second), fx.now())
	require.NoError(t, err)
	ok, err := c.HasOpenOnceWakeSchedule(fx.ctx, "sess-1")
	require.NoError(t, err)
	require.True(t, ok, "an active once schedule must hold the run open")

	require.NoError(t, fx.store.CancelSchedule(fx.ctx, "sess-1", once.ID, fx.now()))
	ok, err = c.HasOpenOnceWakeSchedule(fx.ctx, "sess-1")
	require.NoError(t, err)
	require.False(t, ok, "a cancelled once schedule holds nothing")

	_, err = fx.store.CreateSchedule(fx.ctx, sessionwake.CreateWakeScheduleParams{
		Owner: "sess-1", Kind: sessionwake.WakeKindLoop, Message: "tick",
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
	params := buildSessionNoticeMessageParams(sessionnotice.SessionNoticeRow{
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
