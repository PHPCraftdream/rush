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
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
)

var _ tools.WakeControl = (*coordinator)(nil)

const (
	// wakePastTolerance is the clock-skew allowance when rejecting a past
	// at/unil timestamp (contract §1.2: "допуск 2 с").
	wakePastTolerance = 2 * time.Second
	// wakeMaxDelaySeconds is wakein's upper bound (§1.1: 100 years in
	// seconds — a unit-mistake guard, not a product limit).
	wakeMaxDelaySeconds = int64(3155760000)
)

func (c *coordinator) ScheduleWakeOnceDelay(ctx context.Context, sessionID string, delaySeconds int64, message string) (tools.WakeScheduleView, error) {
	return newWakeComponent(c).ScheduleWakeOnceDelay(ctx, sessionID, delaySeconds, message)
}

func (c *coordinator) ScheduleWakeOnceAt(ctx context.Context, sessionID, at, message string) (tools.WakeScheduleView, error) {
	return newWakeComponent(c).ScheduleWakeOnceAt(ctx, sessionID, at, message)
}

func (c *coordinator) ScheduleWakeLoop(ctx context.Context, sessionID string, everySeconds int64, message string, maxRuns *int64, until *string) (tools.WakeScheduleView, error) {
	return newWakeComponent(c).ScheduleWakeLoop(ctx, sessionID, everySeconds, message, maxRuns, until)
}

func (c *coordinator) ListWakeSchedules(ctx context.Context, sessionID string) ([]tools.WakeScheduleView, error) {
	return newWakeComponent(c).ListWakeSchedules(ctx, sessionID)
}

func (c *coordinator) CancelWakeSchedule(ctx context.Context, sessionID, scheduleID string) (string, error) {
	return newWakeComponent(c).CancelWakeSchedule(ctx, sessionID, scheduleID)
}

func (c *coordinator) wakeSchedulerFor() (*wakeScheduler, error) {
	return newWakeComponent(c).wakeSchedulerFor()
}

// validateWakeMessage enforces §6's message rules (shared by all three
// creators).
func validateWakeMessage(message string) error {
	n := len([]rune(message))
	if n == 0 {
		return errors.New("message is required")
	}
	if n > sessionwake.MaxWakeMessageLength {
		return fmt.Errorf(
			"message exceeds %d characters (%d); shorten it — this becomes part of the next turn's context, not a task payload",
			sessionwake.MaxWakeMessageLength, n,
		)
	}
	return nil
}

// validateWakeDelay enforces §1.1's delay_seconds bounds.
func validateWakeDelay(delaySeconds int64) error {
	if delaySeconds <= 0 {
		return errors.New("delay_seconds must be positive")
	}
	if delaySeconds < 5 {
		return errors.New("delay_seconds must be at least 5 (use it to avoid immediately re-waking yourself)")
	}
	if delaySeconds > wakeMaxDelaySeconds {
		return errors.New("delay_seconds is implausibly large (over 100 years); check your units")
	}
	return nil
}

// parseWakeTimestamp enforces §1.2's RFC3339-with-zone rule.
func parseWakeTimestamp(value, field string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"%s must be RFC3339 with an explicit offset or Z, e.g. %q or %q: %s",
			field, "2026-09-28T09:00:00+03:00", "2026-09-28T06:00:00Z", err,
		)
	}
	return t, nil
}

// checkWakeTimestamp rejects a past timestamp (with the 2s skew allowance)
// and an implausibly far one (§1.2), for both wakeon.at and loop.until.
func checkWakeTimestamp(t time.Time, now time.Time, field string) error {
	if t.Before(now.Add(-wakePastTolerance)) {
		return fmt.Errorf(
			"%s (%s) is in the past (server time is %s); use wakein for a relative delay instead",
			field, t.Format(time.RFC3339), now.Format(time.RFC3339),
		)
	}
	if t.Sub(now) > sessionwake.MaxWakeHorizon {
		return fmt.Errorf("%s is implausibly far in the future", field)
	}
	if t.Sub(now) < sessionwake.MinWakeOnceDelay {
		return fmt.Errorf("%s is too soon (schedule at least %s ahead); use wakein for a short delay", field, sessionwake.MinWakeOnceDelay)
	}
	return nil
}
