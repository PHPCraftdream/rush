package state

import (
	"context"
	"time"
)

// ClosureScope tracks the facts that fence terminal reporting.
type ClosureScope interface {
	TurnEnded()
	ClosedNow() bool
	Descendants(live, complete bool)
	WaitChange(context.Context) error
	MarkClosed() bool
}

// WaitForClosure waits for the turn and confirms descendant closure even
// after stop cancellation. Only the caller that marks closure may report.
func WaitForClosure[P, I any](ctx context.Context, done <-chan struct{}, scope ClosureScope, poll time.Duration, snapshot func() RunRequest[P, I], read func(context.Context, RunRequest[P, I]) (bool, bool)) bool {
	select {
	case <-done:
	case <-ctx.Done():
		<-done
	}
	scope.TurnEnded()
	if poll <= 0 {
		poll = 10 * time.Millisecond
	}
	for !scope.ClosedNow() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), poll)
		req := snapshot()
		live, complete := read(dctx, req)
		if req.NoChildSelected {
			live, complete = false, true
		}
		scope.Descendants(live, complete)
		if scope.ClosedNow() {
			cancel()
			break
		}
		scope.WaitChange(dctx)
		cancel()
	}
	return scope.MarkClosed()
}
