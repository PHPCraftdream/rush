package wakectl

import (
	"context"
	"time"
)

// CancellationView is the cancellation-only view of the wake scheduler that the
// await-timer cleanup holds: cancel one durable schedule, read the scheduler's
// clock, and wake its worker after a local cancellation. The full worker stays
// owned by Scheduler.
type CancellationView struct {
	store interface {
		CancelSchedule(context.Context, string, string, time.Time) error
	}
	now    func() time.Time
	notify func()
}

// NewCancellationView builds the view over the scheduler's own three halves.
func NewCancellationView(s *Scheduler) *CancellationView {
	return &CancellationView{store: s, now: s.Now, notify: s.Notify}
}

// Notify wakes the scheduler worker.
func (s *CancellationView) Notify() { s.notify() }

// CancelSchedule cancels one durable wake schedule by owner and id at the
// scheduler's own clock.
func (s *CancellationView) CancelSchedule(ctx context.Context, owner, id string) error {
	return s.store.CancelSchedule(ctx, owner, id, s.now())
}
