// Coordinator-side implementation of tools.WakeControl — the
// wakein/wakeon/loop/wake_list/wake_cancel tool family (stage 4c of
// docs/plans/2026-09-24-agent-wakes-and-async-job-control.md; contract:
// docs/plans/2026-09-27-wake-tools-contract.md §1/§5/§6). Validation follows
// §1's exact error texts; persistence goes through the stage-4a
// WakeScheduleStore (SCHED-01..05) and every successful mutation recomputes
// the stage-4b worker's timer via Notify (SCHED-06). A coordinator without a
// running worker (SDK/test coordinators) answers with an honest "scheduler
// unavailable" error instead of silently accepting a schedule nobody will
// ever fire.
package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

var _ tools.WakeControl = (*coordinator)(nil)

func (c *coordinator) ScheduleWakeOnceDelay(ctx context.Context, sessionID string, delaySeconds int64, message string) (tools.WakeScheduleView, error) {
	return c.wakeWorker().ScheduleWakeOnceDelay(ctx, sessionID, delaySeconds, message)
}

func (c *coordinator) ScheduleWakeOnceAt(ctx context.Context, sessionID, at, message string) (tools.WakeScheduleView, error) {
	return c.wakeWorker().ScheduleWakeOnceAt(ctx, sessionID, at, message)
}

func (c *coordinator) ScheduleWakeLoop(ctx context.Context, sessionID string, everySeconds int64, message string, maxRuns *int64, until *string) (tools.WakeScheduleView, error) {
	return c.wakeWorker().ScheduleWakeLoop(ctx, sessionID, everySeconds, message, maxRuns, until)
}

func (c *coordinator) ListWakeSchedules(ctx context.Context, sessionID string) ([]tools.WakeScheduleView, error) {
	return c.wakeWorker().ListWakeSchedules(ctx, sessionID)
}

func (c *coordinator) CancelWakeSchedule(ctx context.Context, sessionID, scheduleID string) (string, error) {
	return c.wakeWorker().CancelWakeSchedule(ctx, sessionID, scheduleID)
}
