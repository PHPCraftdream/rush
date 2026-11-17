// wakeSession: the single wake primitive (docs/plans/2026-09-28-async-
// phase4-durable-core.md sec.3.4, attempts design 1.6). It persists nothing
// itself: by the time a caller reaches it the fact is durably committed (a
// batch completion row or a session_notices row). Its job
// is to decide -- through the ONE launch predicate drainDecision -- whether a
// Drain call is submitted to the session's owning driver: idle becomes owner
// like any call, busy queues (mailbox_queue.go's mergeQueuedCall). The
// Drain's own turn-start pull moves the notice into history. Nothing runs
// after Run returns: the attempt is accounted by the turn loop that ran it
// (drain_attempt.go).
package agent

import (
	"context"
)

func (c *coordinator) wakeSession(ctx context.Context, sessionID string, fact bool) error {
	return newWakeComponent(c).wakeSession(ctx, sessionID, fact)
}

func (c *coordinator) handleDrainBuildError(sessionID string, buildErr error) error {
	return newWakeComponent(c).handleDrainBuildError(sessionID, buildErr)
}
