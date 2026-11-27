package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
)

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

// The verdict read through the port must map to the same drain verdict as the
// arbiter verdict read directly: kind, reason and the gate-pause retry time.
func TestCoordinatorPortVerdictMapsLikeDirectVerdict(t *testing.T) {
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	for _, v := range []drainctl.Verdict{
		{Kind: drainctl.VRun},
		{Kind: drainctl.VNone, Reason: "no debt"},
		{Kind: drainctl.VClose, Reason: "settle"},
		{Kind: drainctl.VDefer, Reason: "rerun in progress"},
		{Kind: drainctl.VDefer, Reason: "retry pause after an unreacted attempt", RecheckAt: at},
		{Kind: drainctl.VDefer, Reason: "repeated unreacted attempts", RecheckAt: at},
		{Kind: drainctl.VDefer, Reason: "another process drives the session"},
		{Kind: drainctl.VDefer, Reason: "auto-resume off"},
	} {
		require.Equal(t, drainVerdictOf(v), drainVerdictFromPort(coordinatorVerdictAdapter{v}), "%v %q", v.Kind, v.Reason)
	}

	require.Equal(t, drainAllow, drainVerdictFromPort(coordinatorVerdictAdapter{drainctl.Verdict{Kind: drainctl.VNone}}).Kind,
		"nothing to decide is an allow, not a dormant gate")
	paced := drainVerdictFromPort(coordinatorVerdictAdapter{drainctl.Verdict{
		Kind: drainctl.VDefer, Reason: "retry pause after an unreacted attempt", RecheckAt: at,
	}})
	require.Equal(t, drainctl.DrainPaced, paced.Kind)
	require.Equal(t, at, paced.RetryAt, "the gate-pause retry time survives the port")
}
