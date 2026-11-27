package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
)

func TestAwaitTasksMaxWaitArbiterIndependent(t *testing.T) {
	for _, all := range []bool{false, true} {
		a := &arbiter{}
		if all {
			a.setSleepAll("owner")
		}
		a.setMaxWaitWake("owner", "timer")
		require.Equal(t, all, a.snapshot("owner").SleepAll)
		require.False(t, a.state("owner").idle(time.Now()), "pending timer retains the state")
		require.Equal(t, "timer", a.clearSleepAll("owner"))
		require.False(t, a.snapshot("owner").SleepAll)
		require.Empty(t, a.clearSleepAll("owner"), "timer is consumed once")
		require.True(t, a.state("owner").idle(time.Now()))
	}
}

func TestAwaitTasksMaxWaitClearTerminalQuiet(t *testing.T) {
	fx := newSchedFx(t)
	c := &coordinator{wakeScheduler: fx.scheduler()}
	for _, state := range []string{"active", "done", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			row, err := fx.store.CreateSchedule(fx.ctx, sessionwake.CreateWakeScheduleParams{
				Owner: "owner", Kind: sessionwake.WakeKindOnce, Message: sessionwake.AwaitTasksMaxWaitPrefix,
				RunAt: fx.now().Add(time.Hour),
			}, fx.now())
			require.NoError(t, err)
			_, err = fx.conn.ExecContext(fx.ctx, `UPDATE wake_schedules SET state = ? WHERE id = ?`, state, row.ID)
			require.NoError(t, err)
			c.arb.setMaxWaitWake("owner", row.ID)
			c.clearSleepAll("owner")
			require.Empty(t, c.arb.clearSleepAll("owner"))
			got, err := fx.q.GetWakeSchedule(fx.ctx, row.ID)
			require.NoError(t, err)
			if state == "active" {
				state = "cancelled"
			}
			require.Equal(t, state, got.State)
		})
	}
}

func TestAwaitTasksMaxWaitSweepSelective(t *testing.T) {
	fx := newSchedFx(t)
	now := fx.now()
	type scenario struct {
		owner, message, state string
		kind                  sessionwake.WakeKind
	}
	cases := []scenario{
		{"owner", sessionwake.AwaitTasksMaxWaitPrefix + " old", "active", sessionwake.WakeKindOnce},
		{"other", sessionwake.AwaitTasksMaxWaitPrefix + " foreign", "active", sessionwake.WakeKindOnce},
		{"owner", "model-owned timer", "active", sessionwake.WakeKindOnce},
		{"owner", "x" + sessionwake.AwaitTasksMaxWaitPrefix, "active", sessionwake.WakeKindOnce},
		{"owner", sessionwake.AwaitTasksMaxWaitPrefix, "active", sessionwake.WakeKindLoop},
		{"owner", sessionwake.AwaitTasksMaxWaitPrefix, "done", sessionwake.WakeKindOnce},
		{"owner", sessionwake.AwaitTasksMaxWaitPrefix, "cancelled", sessionwake.WakeKindOnce},
	}
	ids := make([]string, 0, len(cases))
	for _, tc := range cases {
		p := sessionwake.CreateWakeScheduleParams{Owner: tc.owner, Kind: tc.kind, Message: tc.message, RunAt: now.Add(time.Hour)}
		if tc.kind == sessionwake.WakeKindLoop {
			p.Every = 300 * time.Second
		}
		row, err := fx.store.CreateSchedule(fx.ctx, p, now)
		require.NoError(t, err)
		_, err = fx.conn.ExecContext(fx.ctx, `UPDATE wake_schedules SET state = ? WHERE id = ?`, tc.state, row.ID)
		require.NoError(t, err)
		ids = append(ids, row.ID)
	}
	n, err := fx.store.CancelAwaitTasksMaxWait(fx.ctx, "owner", now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	for i, id := range ids {
		row, err := fx.q.GetWakeSchedule(fx.ctx, id)
		require.NoError(t, err)
		if i == 0 {
			require.Equal(t, "cancelled", row.State)
			require.Equal(t, now.Add(time.Minute).Unix(), row.UpdatedAt)
		} else {
			require.Equal(t, cases[i].state, row.State)
			require.Equal(t, now.Unix(), row.UpdatedAt, "excluded rows must not be touched")
		}
	}
	n, err = fx.store.CancelAwaitTasksMaxWait(fx.ctx, "owner", now.Add(2*time.Minute))
	require.NoError(t, err)
	require.Zero(t, n)
	row, err := fx.q.GetWakeSchedule(fx.ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Minute).Unix(), row.UpdatedAt, "no double touch")
}

// The port's ClearSleepAll must cancel the pending max_wait schedule, not only
// disarm the sleep flag (the pre-port call was coordinator.clearSleepAll).
func TestCoordinatorPortClearSleepAllCancelsMaxWaitSchedule(t *testing.T) {
	fx := newSchedFx(t)
	c := &coordinator{wakeScheduler: fx.scheduler()}
	row, err := fx.store.CreateSchedule(fx.ctx, sessionwake.CreateWakeScheduleParams{
		Owner: "owner", Kind: sessionwake.WakeKindOnce, Message: sessionwake.AwaitTasksMaxWaitPrefix,
		RunAt: fx.now().Add(time.Hour),
	}, fx.now())
	require.NoError(t, err)
	c.arb.setSleepAll("owner")
	c.arb.setMaxWaitWake("owner", row.ID)

	coordinatorPortFrom(c).ClearSleepAll("owner")

	require.False(t, c.arb.snapshot("owner").SleepAll, "the sleep flag is disarmed")
	got, err := fx.q.GetWakeSchedule(fx.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, "cancelled", got.State, "the max_wait schedule is cancelled by the port clear")
}
