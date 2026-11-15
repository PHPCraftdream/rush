package nettransport

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// connectHandshakeTimeout bounds the CONNECT handshake phase (request
// write plus response read) on an established proxy connection. The
// TCP dial has its own net.Dialer.Timeout; this budget covers what
// happens after the proxy has accepted the connection, where a silent
// or slow proxy would otherwise hold the dial open forever.
const connectHandshakeTimeout = 30 * time.Second

// dnsQueryTimeout bounds the write/read phase of one DNS-over-TCP
// query, including the time the query spends tunneled through a proxy.
const dnsQueryTimeout = 30 * time.Second

// socksHandshakeTimeout bounds one SOCKS5 exchange end to end: the TCP
// dial to the proxy plus the greeting, optional authentication, and
// CONNECT round trip that x/net performs over the raw connection. It
// is a var only so tests can shorten it through BuildHTTPClient, which
// has no parameter channel for it; production code must never assign
// it, and the tests that do are serial (no t.Parallel).
//
// The budget is synthesized as a context deadline rather than enforced
// by handshakeGuard: x/net's SOCKS client sets a deadline on the raw
// socket only when the context already carries one (internal/socks
// client.go, connect), and clears it again before returning the
// established tunnel, while the stop() join point of a guard sits
// exactly where x/net — not Rush — owns the connection. The budget
// must also be owned outright: net/http detaches the dial context
// from the request's cancellation (getConn's context.WithoutCancel),
// so neither caller cancellation nor any client timeout would
// otherwise bound a silent proxy.
var socksHandshakeTimeout = 30 * time.Second

// handshakeGuard bounds a blocking handshake phase on an established
// connection. The phase's Write/Read calls take no context, so the
// guard bounds them twice: a provisional deadline on the raw socket,
// and a watcher that closes the socket as soon as the handshake's
// context is cancelled or the deadline expires. stop MUST be called on
// every return path; on the success path it JOINS the watcher and
// clears the deadline before the connection is handed back, so a
// cancelled dial can never close a tunnel that was already returned as
// good. A non-nil stop error means the watcher closed the socket first
// — the handshake result, however successful it looked, is void.
type handshakeGuard struct {
	conn     net.Conn
	stopCh   chan struct{}
	done     chan struct{}
	abortErr atomic.Pointer[abortError]
}

// abortError records why the watcher closed the connection.
type abortError struct {
	cause error
}

func (a *abortError) Error() string { return a.cause.Error() }

// armHandshakeGuard sets the provisional deadline and starts the
// cancellation watcher for conn.
func armHandshakeGuard(ctx context.Context, conn net.Conn, timeout time.Duration) *handshakeGuard {
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)
	g := &handshakeGuard{
		conn:   conn,
		stopCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	go func() {
		defer close(g.done)
		timer := time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		var cause error
		select {
		case <-ctx.Done():
			cause = ctx.Err()
		case <-timer.C:
			cause = fmt.Errorf("handshake deadline (%s) expired", timeout)
		case <-g.stopCh:
			return
		}
		g.abortErr.Store(&abortError{cause: cause})
		_ = conn.Close()
	}()
	return g
}

// stop joins the watcher and, unless the watcher already closed the
// connection, clears the provisional deadline so the caller receives
// an unmodified, unbounded connection. A non-nil return reports why
// the watcher closed the connection: context cancellation or deadline
// expiry won the race against a handshake that may still have looked
// successful.
func (g *handshakeGuard) stop() error {
	close(g.stopCh)
	<-g.done
	if abort := g.abortErr.Load(); abort != nil {
		return fmt.Errorf("handshake aborted: %w", abort.cause)
	}
	_ = g.conn.SetDeadline(time.Time{})
	return nil
}
