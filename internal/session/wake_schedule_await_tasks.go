package session

import (
	"context"
	"strings"
	"time"
)

// AwaitTasksMaxWaitPrefix identifies internal await_tasks timers, not model
// timers. It is shared by creation and run-start orphan cleanup.
const AwaitTasksMaxWaitPrefix = "await_tasks max_wait elapsed:"

// CancelAwaitTasksMaxWait cancels only owner's active internal once timers.
// Terminal rows are skipped and the cancellation CAS tolerates a concurrent
// fire. Ordinary once and loop schedules are never affected.
func (s *WakeScheduleStore) CancelAwaitTasksMaxWait(ctx context.Context, owner string, now time.Time) (int, error) {
	rows, err := s.ListSchedules(ctx, owner)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, row := range rows {
		if row.State != "active" || row.Kind != string(WakeKindOnce) || !strings.HasPrefix(row.Message, AwaitTasksMaxWaitPrefix) {
			continue
		}
		changed, err := s.CancelScheduleReport(ctx, owner, row.ID, now)
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	return n, nil
}
