package server

import (
	"context"
)

func apiSSERevocation(ctx context.Context, principal Principal) <-chan struct{} {
	revoked := make(chan struct{})
	factory := principal.TickerFactory()
	if factory == nil {
		factory = RealTicker
	}
	go func() {
		ticker := factory(authRecheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C():
				if !StillValid(principal, now) {
					close(revoked)
					return
				}
			}
		}
	}()
	return revoked
}
