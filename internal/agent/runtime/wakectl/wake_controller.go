package wakectl

import (
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
)

// WakeScheduleController is the optional interface the App asserts the
// Coordinator to when wiring the stage-4b wake-schedule worker
// (internal/app). Deliberately NOT part of the Coordinator interface
// itself: every test fake implementing Coordinator would have to grow the
// two methods for nothing (same reasoning as
// coordinator_work_scope.go's own not-on-the-interface note).
type WakeScheduleController interface {
	// SetWakeScheduleStore hands the coordinator the durable wake-schedule
	// store and starts the stage-4b worker over it. nil (or a second call)
	// is a no-op.
	SetWakeScheduleStore(store *sessionwake.WakeScheduleStore)
	// StopWakeScheduler terminates the worker; safe to call repeatedly
	// and on a coordinator that never got a store.
	StopWakeScheduler()
}
