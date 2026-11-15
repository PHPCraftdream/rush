package server

import (
	"context"
	"time"
)

func apiSSERevocation(ctx context.Context, principal Principal) <-chan struct{} {
	revoked := make(chan struct{})
	factory := principal.tickerFactory
	if factory == nil {
		factory = func(d time.Duration) Ticker { return realTicker{t: time.NewTicker(d)} }
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
