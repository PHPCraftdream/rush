package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAwaitTimerEraRejectsLateInstall(t *testing.T) {
	for _, action := range []string{"clear", "stop", "human_reset", "replacement", "cancel"} {
		t.Run(action, func(t *testing.T) {
			fx := newSchedFx(t)
			trace := batchAgentTraceFor(t)
			c := &coordinator{wakeScheduler: newWakeScheduler(fx.store, "test", nil)}
			c.wakeScheduler.now = fx.now
			ctx, cancel := context.WithCancel(fx.ctx)
			defer cancel()
			model, err := fx.store.CreateSchedule(fx.ctx, session.CreateWakeScheduleParams{Owner: "owner", Kind: session.WakeKindOnce, Message: "model timer", RunAt: fx.now().Add(time.Hour)}, fx.now())
			require.NoError(t, err)
			create := func() (string, error) {
				v, err := c.ScheduleWakeOnceDelay(fx.ctx, "owner", 3600, session.AwaitTasksMaxWaitPrefix)
				trace.addf("timer created: id=%s err=%v", v.ScheduleID, err)
				return v.ScheduleID, err
			}
			var stale, current string
			id, err := c.armAwait(ctx, "owner", true, func() (string, error) {
				var err error
				stale, err = create()
				if err != nil {
					return stale, err
				}
				trace.addf("before late install: %s", action)
				switch action {
				case "clear":
					c.clearSleepAll("owner")
				case "stop":
					c.arb.suspend("owner")
					c.clearSleepAll("owner")
				case "human_reset":
					c.resetConsecutiveResume("owner")
				case "cancel":
					cancel()
				case "replacement":
					current, err = c.armAwait(fx.ctx, "owner", true, create)
				}
				return stale, err
			})
			require.Error(t, err)
			require.Empty(t, id)
			old, err := fx.q.GetWakeSchedule(fx.ctx, stale)
			require.NoError(t, err)
			require.Equal(t, "cancelled", old.State, "late timer must not retain OnceWakeOpen")
			require.Equal(t, action == "replacement", c.arb.snapshot("owner").SleepAll)
			if current != "" {
				row, err := fx.q.GetWakeSchedule(fx.ctx, current)
				require.NoError(t, err)
				require.Equal(t, "active", row.State, "old install must not cancel newer timer")
			}
			c.clearSleepAll("owner")
			rows, err := fx.store.ListSchedules(fx.ctx, "owner")
			require.NoError(t, err)
			for _, row := range rows {
				trace.addf("final timer: id=%s state=%s", row.ID, row.State)
				if row.ID == model.ID {
					require.Equal(t, "active", row.State)
				} else {
					require.NotEqual(t, "active", row.State)
				}
			}
		})
	}
}

func TestAwaitTimerEraReplacesOldTimer(t *testing.T) {
	fx := newSchedFx(t)
	trace := batchAgentTraceFor(t)
	c := &coordinator{wakeScheduler: newWakeScheduler(fx.store, "test", nil)}
	c.wakeScheduler.now = fx.now
	create := func() (string, error) {
		v, err := c.ScheduleWakeOnceDelay(fx.ctx, "owner", 3600, session.AwaitTasksMaxWaitPrefix)
		trace.addf("create id=%s err=%v", v.ScheduleID, err)
		return v.ScheduleID, err
	}
	first, err := c.armAwait(fx.ctx, "owner", true, create)
	require.NoError(t, err)
	second, err := c.armAwait(fx.ctx, "owner", false, create)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
	row, err := fx.q.GetWakeSchedule(fx.ctx, first)
	require.NoError(t, err)
	require.Equal(t, "cancelled", row.State)
	c.clearSleepAll("owner")
	row, err = fx.q.GetWakeSchedule(fx.ctx, second)
	require.NoError(t, err)
	require.Equal(t, "cancelled", row.State)
}

func TestAwaitTimerEraOldFailureCannotClearNewSleep(t *testing.T) {
	fx := newSchedFx(t)
	trace := batchAgentTraceFor(t)
	c := &coordinator{wakeScheduler: newWakeScheduler(fx.store, "test", nil)}
	c.wakeScheduler.now = fx.now
	var current string
	_, err := c.armAwait(fx.ctx, "owner", true, func() (string, error) {
		var err error
		current, err = c.armAwait(fx.ctx, "owner", true, func() (string, error) {
			v, err := c.ScheduleWakeOnceDelay(fx.ctx, "owner", 3600, session.AwaitTasksMaxWaitPrefix)
			return v.ScheduleID, err
		})
		if err != nil {
			return "", err
		}
		trace.addf("new timer committed before old error: %s", current)
		return "", errors.New("old create failed")
	})
	require.EqualError(t, err, "old create failed")
	require.True(t, c.arb.snapshot("owner").SleepAll)
	row, err := fx.q.GetWakeSchedule(fx.ctx, current)
	require.NoError(t, err)
	require.Equal(t, "active", row.State)
	c.clearSleepAll("owner")
	row, err = fx.q.GetWakeSchedule(fx.ctx, current)
	require.NoError(t, err)
	require.Equal(t, "cancelled", row.State)
}

func TestAwaitTimerEraHumanResetReturnsTimerAtomically(t *testing.T) {
	trace := batchAgentTraceFor(t)
	a := &arbiter{}
	first, _ := a.beginAwait("owner", true)
	require.True(t, a.installAwait("owner", first, "old-timer"))
	require.Equal(t, "old-timer", a.resetForHumanMessage("owner"))
	second, _ := a.beginAwait("owner", true)
	trace.addf("reset deleted old state; new era installed")
	require.True(t, a.installAwait("owner", second, "new-timer"))
	require.False(t, a.installAwait("owner", first, "late-old-timer"), "deleted/recreated state must not admit old era")
	a.abortAwait("owner", first)
	require.True(t, a.snapshot("owner").SleepAll)
	require.Equal(t, "new-timer", a.clearSleepAll("owner"))
}

func TestAwaitTimerEraCannotArmStoppedSession(t *testing.T) {
	trace := batchAgentTraceFor(t)
	c := &coordinator{}
	c.arb.suspend("owner")
	called := false
	_, err := c.armAwait(t.Context(), "owner", true, func() (string, error) {
		called = true
		return "forbidden-timer", nil
	})
	trace.addf("arm stopped session: called=%v err=%v", called, err)
	require.ErrorIs(t, err, errAwaitSuperseded)
	require.False(t, called)
	require.False(t, c.arb.snapshot("owner").SleepAll)
}
