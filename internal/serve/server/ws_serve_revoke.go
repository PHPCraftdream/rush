package server

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
)

const authRecheckInterval = 15 * time.Second

func watchServeRevocation(ctx context.Context, conn *websocket.Conn, principal Principal, interval time.Duration, factory func(time.Duration) Ticker) {
	if factory == nil {
		factory = RealTicker
	}
	ticker := factory(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C():
			if !StillValid(principal, now) {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "token revoked or expired"), time.Now().Add(time.Second))
				_ = conn.Close()
				return
			}
		}
	}
}

func startServeRevocationWatch(ctx context.Context, conn *websocket.Conn, principal Principal) {
	if principal.TickerFactory() != nil {
		go watchServeRevocation(ctx, conn, principal, authRecheckInterval, principal.TickerFactory())
		return
	}
	go watchServeRevocation(ctx, conn, principal, authRecheckInterval, nil)
}
