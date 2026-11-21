// Stage 5a: the wake-schedule side of the CLI lifetime policy (operator
// decisions, docs/plans/2026-09-24-agent-wakes-and-async-job-control.md
// sec.5): an ACTIVE once schedule is open work that holds `rush run` open,
// an ACTIVE loop schedule is not -- the CLI loop cancels the session's
// loop schedules when its scope closes with nothing else open. Storage
// reads/writes stay in session.WakeScheduleStore (SCHED-05); this file only
// filters and cancels through it.
package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/store/db"
)

// LoopWakeCanceler is the optional interface the CLI loop asserts its
// ReactionDebtSource to (not part of ReactionDebtSource itself: every test
// fake would have to grow the method for nothing).
type LoopWakeCanceler interface {
	// CancelLoopWakeSchedules cancels every ACTIVE loop schedule owned by
	// sessionID and returns how many rows it moved to cancelled.
	CancelLoopWakeSchedules(ctx context.Context, sessionID string) (int, error)
}

func (c *coordinator) openOnceWakeSchedules(ctx context.Context, sessionID string) ([]db.WakeSchedule, error) {
	return c.wakeWorker().OpenOnceWakeSchedules(ctx, sessionID)
}

func (c *coordinator) HasOpenOnceWakeSchedule(ctx context.Context, sessionID string) (bool, error) {
	once, err := c.openOnceWakeSchedules(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return len(once) > 0, nil
}

func (c *coordinator) CancelLoopWakeSchedules(ctx context.Context, sessionID string) (int, error) {
	return c.wakeWorker().CancelLoopWakeSchedules(ctx, sessionID)
}
