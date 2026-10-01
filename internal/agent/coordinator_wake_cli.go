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
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// LoopWakeCanceler is the optional interface the CLI loop asserts its
// ReactionDebtSource to (not part of ReactionDebtSource itself: every test
// fake would have to grow the method for nothing).
type LoopWakeCanceler interface {
	// CancelLoopWakeSchedules cancels every ACTIVE loop schedule owned by
	// sessionID and returns how many rows it moved to cancelled.
	CancelLoopWakeSchedules(ctx context.Context, sessionID string) (int, error)
}

// openOnceWakeSchedules lists sessionID's ACTIVE once schedules, soonest
// first -- the stage-5a "should `rush run` hold the process open" set. An
// unreadable store is an error the caller surfaces, never a silent "none".
func (c *coordinator) openOnceWakeSchedules(ctx context.Context, sessionID string) ([]db.WakeSchedule, error) {
	if c == nil || c.wakeScheduler == nil {
		return nil, nil
	}
	rows, err := c.wakeScheduler.store.ListSchedules(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var once []db.WakeSchedule
	for _, row := range rows {
		if row.Kind == string(session.WakeKindOnce) && row.State == "active" {
			once = append(once, row)
		}
	}
	return once, nil
}

// HasOpenOnceWakeSchedule reports whether sessionID owns at least one
// ACTIVE once schedule -- the stage-5a hold predicate (operator decision:
// a one-shot timer holds the run, a loop does not).
func (c *coordinator) HasOpenOnceWakeSchedule(ctx context.Context, sessionID string) (bool, error) {
	once, err := c.openOnceWakeSchedules(ctx, sessionID)
	if err != nil {
		return false, err
	}
	return len(once) > 0, nil
}

// CancelLoopWakeSchedules implements LoopWakeCanceler: every ACTIVE loop
// schedule of sessionID moves to cancelled (SCHED-05's owner-checked CAS;
// already terminal rows are no-ops), and the worker's timer is recomputed.
// Once schedules are never touched: they hold the process, so the close
// path cannot be running while one is open.
func (c *coordinator) CancelLoopWakeSchedules(ctx context.Context, sessionID string) (int, error) {
	if c == nil || c.wakeScheduler == nil {
		return 0, nil
	}
	rows, err := c.wakeScheduler.store.ListSchedules(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	n := 0
	now := time.Now()
	for _, row := range rows {
		if row.Kind != string(session.WakeKindLoop) || row.State != "active" {
			continue
		}
		if err := c.wakeScheduler.store.CancelSchedule(ctx, sessionID, row.ID, now); err != nil {
			return n, err
		}
		n++
	}
	if n > 0 {
		c.WakeSchedulerNotify()
	}
	return n, nil
}
