package hub

// gate is the S4 WS-gateway component (design §3): handleWS/readPump
// (server.go:504-647 at HEAD) plus ws_serve_revoke.go moved verbatim; every
// former s.X read became the corresponding GateOptions field/constructor
// binding. The composer (server.go mux "/ws" section) mounts the gate
// directly via hub.NewGate — no delegating methods remain on Server.

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
)

// GateOptions carries the composer-provided collaborators. Ctx is required
// because handleWS's register send must abandon on the server-lifetime context
// (composer Start's runCtx); OnIncoming keeps the dispatch switch in package
// server, called by readPump through this hook.
type GateOptions struct {
	Hub         *Hub
	Ctx         context.Context
	CheckOrigin func(*http.Request) bool
	UIAuth      bool
	AllowAdmin  bool
	OnIncoming  func(ctx context.Context, c *Client, raw []byte)
	BeforeWrite func([]byte)
	Workers     *sync.WaitGroup
}

// gate serves the "/ws" endpoint: upgrade, hello, revocation watch, client
// construction, registration and the read loop.
type gate struct {
	hub         *Hub
	ctx         context.Context
	checkOrigin func(*http.Request) bool
	uiAuth      bool
	allowAdmin  bool
	onIncoming  func(context.Context, *Client, []byte)
	beforeWrite func([]byte)
	workers     *sync.WaitGroup
}

// NewGate builds the WS gateway. Always non-nil.
func NewGate(opts GateOptions) http.Handler {
	return &gate{
		hub:         opts.Hub,
		ctx:         opts.Ctx,
		checkOrigin: opts.CheckOrigin,
		uiAuth:      opts.UIAuth,
		allowAdmin:  opts.AllowAdmin,
		onIncoming:  opts.OnIncoming,
		beforeWrite: opts.BeforeWrite,
		workers:     opts.Workers,
	}
}

// ServeHTTP upgrades and serves one WebSocket connection (former
// Server.handleWS entry point).
func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.handleWS(g.ctx, w, r)
}

// ctx is the server's lifetime context (Start's), so the register send
// below can be abandoned when the hub has already stopped.
func (g *gate) handleWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 64 * 1024,
		CheckOrigin:     g.checkOrigin,
	}
	if !g.uiAuth {
		// Gorilla's Upgrader reads only Header.Get for this field. Normalize
		// repeated header lines so all client-offered protocols are considered.
		r = r.Clone(r.Context())
		protocols := r.Header.Values("Sec-WebSocket-Protocol")
		if len(protocols) > 1 {
			r.Header.Set("Sec-WebSocket-Protocol", strings.Join(protocols, ","))
		}
		upgrader.Subprotocols = []string{"rush.v1"}
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws: upgrade failed", "err", err)
		return
	}

	if !g.uiAuth {
		if principal, ok := apiauth.PrincipalFrom(r.Context()); ok {
			if err := conn.WriteJSON(wire.WSMessage{Type: wire.EventHello, Payload: wire.MustJSONRaw(wire.HelloPayload{Protocol: "rush.v1", TokenID: principal.TokenID, Scopes: principal.Scopes})}); err != nil {
				_ = conn.Close()
				return
			}
			startServeRevocationWatch(r.Context(), conn, principal)
		}
	}
	c := newClient(g.hub, conn)
	c.beforeWrite = g.beforeWrite
	if g.uiAuth {
		// The UI listener is the only transport that grants the
		// trusted-UI settings-write authority: Auth.Middleware has
		// already verified the operator's cookie/token for this
		// connection before the upgrade completed.
		c.trustedSettingsWrite = true
	}
	if !g.uiAuth {
		if principal, ok := apiauth.PrincipalFrom(r.Context()); ok {
			c.auth = NewClientAuth(principal, CommandPolicy{AllowAdmin: g.allowAdmin})
		}
	}
	g.workers.Add(1)
	go func() {
		defer g.workers.Done()
		c.workers.Wait()
	}()
	// register is buffered (64) but nothing drains it once Hub.Run has
	// returned, and a bare select between the send and ctx.Done() cannot
	// close the shutdown race: with buffer space left and ctx cancelled,
	// Go picks a branch at random, so the send can strand the client in a
	// channel nobody reads (task #723). tryRegister checks the hub's
	// explicit stopped signal — closed before Run's final register drain —
	// both before and after the send, which makes the check airtight. On
	// false, tear the just-constructed client down the way readPump's
	// defers would have: close the socket and stop newClient's worker
	// pool. No unregister send — the hub is gone, nothing would read it.
	if !g.hub.TryRegister(ctx, c) {
		_ = conn.Close()
		close(c.workQueue)
		return
	}

	// Start write pump in background; read pump blocks this goroutine.
	go c.writePump()
	g.readPump(r.Context(), c)
}

// readPump reads messages from the WebSocket and dispatches them.
func (g *gate) readPump(ctx context.Context, c *Client) {
	// c.conn.Close() runs unconditionally in its own defer, independent of
	// whether the unregister send below completes. Hub.Run stops reading
	// from unregister once ctx is cancelled (see its ctx.Done() case), so
	// on shutdown, once the buffered channel (cap 64) fills up, sending
	// would block this goroutine forever — and with it, the deferred
	// conn.Close() that never runs, leaking both the goroutine and the
	// socket. Splitting the close into its own unconditional defer means
	// the connection is always torn down even when the unregister send
	// below is abandoned via ctx.Done().
	defer c.conn.Close()
	defer func() {
		select {
		case g.hub.unregister <- c:
		case <-g.hub.stopped:
		case <-ctx.Done():
		}
	}()
	// Closing workQueue stops this connection's fixed worker pool (see
	// startWorkers in hub.go) once no more items will ever be enqueued.
	// Safe here specifically because dispatch is only ever called
	// synchronously from within this function's read loop below — by the
	// time this defer runs, the loop has already exited, so nothing can
	// still be sending on workQueue concurrently with this close.
	//
	// DECISION (task #698 review): items already queued — including
	// still-buffered ones, since `for range` drains a closed channel's
	// buffer before exiting — are deliberately allowed to run to
	// completion after the connection is gone. This is intended
	// decoupling, not a leak: agent turns are session-scoped, not
	// connection-scoped (the agent-driving handlers run the coordinator
	// under context.WithoutCancel precisely so a browser refresh
	// mid-turn does not cancel the run — see handlers_agent.go), and a
	// reconnecting client re-attaches via the sessions_list poll plus
	// the hub's event replay. The blast radius is bounded
	// (workQueueDepth items, panic-isolated by runRecovered) and
	// post-teardown replies are harmless (Client.reply recovers the
	// closed-send-channel case). Do not "fix" this by draining-and-
	// dropping the queue on disconnect.
	defer close(c.workQueue)

	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Fork merge note (origin/main 9c35ee01 "fix(server): recover from handler panics"):
	// upstream wraps the REST mux with recoverHandler; our WebSocket loop reads
	// frames directly. The equivalent in our architecture lives one layer down
	// from handleIncoming: every message handler is handed to Client.dispatch
	// (hub.go), which enqueues it on a per-connection bounded work queue
	// drained by a fixed worker pool whose runRecovered recovers any panic —
	// with the control-plane exception (CmdCancelAgent/CmdInterruptAndSend/
	// CmdShutdownServer) going through Client.dispatchControl, which does spawn
	// a goroutine per command, semaphore-bounded, under the same runRecovered net.
	// No handler runs as a bare `go handleX(...)` off the read loop. See
	// CHANGELOG.fork.md section 4.A.
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Debug("ws: read error", "err", err)
			}
			return
		}
		g.onIncoming(ctx, c, raw)
	}
}

const authRecheckInterval = 15 * time.Second

func watchServeRevocation(ctx context.Context, conn *websocket.Conn, principal apiauth.Principal, interval time.Duration, factory func(time.Duration) apiauth.Ticker) {
	if factory == nil {
		factory = apiauth.RealTicker
	}
	ticker := factory(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C():
			if !apiauth.StillValid(principal, now) {
				_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "token revoked or expired"), time.Now().Add(time.Second))
				_ = conn.Close()
				return
			}
		}
	}
}

func startServeRevocationWatch(ctx context.Context, conn *websocket.Conn, principal apiauth.Principal) {
	if principal.TickerFactory() != nil {
		go watchServeRevocation(ctx, conn, principal, authRecheckInterval, principal.TickerFactory())
		return
	}
	go watchServeRevocation(ctx, conn, principal, authRecheckInterval, nil)
}

// StartServeRevocationWatch is the exported form of the moved unexported
// helper, exported for root call sites: the staying server test
// ws_serve_revoke_test.go:40 calls it directly until L7. Production caller:
// gate.handleWS.
func StartServeRevocationWatch(ctx context.Context, conn *websocket.Conn, principal apiauth.Principal) {
	startServeRevocationWatch(ctx, conn, principal)
}
