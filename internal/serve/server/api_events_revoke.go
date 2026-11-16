package server

import (
	"context"
	"time"
)

// authRecheckInterval is a small local copy (sprint 0 L4 escape rule:
// "small local copy ... report with file:line") of the constant that moved to
// hub/gate.go with ws_serve_revoke.go (ws_serve_revoke.go:9 at HEAD). The SSE
// revocation ticker must tick at the same interval as the WS watch.
const authRecheckInterval = 15 * time.Second

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
