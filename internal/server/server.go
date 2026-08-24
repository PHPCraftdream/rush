// Package server implements the HTTP + WebSocket server for rush's web mode.
// It serves the embedded React application and bridges the app's pubsub
// event system to connected browsers over WebSocket.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/gorilla/websocket"
)

// Server wires together the HTTP mux, the WebSocket hub, and the app.
type Server struct {
	app    *appPkg.App
	hub    *Hub
	auth   *Auth
	addr   string
	static fs.FS

	// port is the actual TCP port the listener bound to. It's populated once
	// Start() binds the listener (useful when addr requested port 0), and is
	// read by checkOrigin to validate the WebSocket handshake's Origin header
	// against the port this server is actually reachable on.
	port string

	// shutdownSig is closed (once) to ask Start()'s shutdown watcher to begin
	// graceful shutdown — the WS-triggered counterpart of ctx.Done(). Guarded
	// lazy init so bare struct literals (tests) work too.
	shutdownMu   sync.Mutex
	shutdownSig  chan struct{}
	shutdownOnce sync.Once
}

// checkOrigin validates the WebSocket handshake's Origin header. This is the
// only thing standing between a page open in the operator's browser and full
// control of the agent over ws://<host>:<port>/ws if it's ever wrong — the
// session cookie's SameSite=Strict is a second layer, but browsers that don't
// enforce SameSite, a relaxed SameSite in the future, or a token leaked into
// ?token= would leave CheckOrigin as the only remaining guard (CSWSH).
//
// Non-browser clients (CLI, scripts, curl, gorilla/websocket dialers) don't
// send an Origin header at all, so an empty Origin is accepted — this is the
// fork's primary non-interactive use case and must keep working.
//
// Browser-originated requests are accepted only from http://localhost:<port>
// or http://127.0.0.1:<port> (and http://[::1]:<port> when applicable),
// where <port> is the port this server actually bound to. This intentionally
// ignores scheme upgrades to https and ignores whatever host the operator
// passed via --host: the WS endpoint is meant to be reached from a browser
// running on the same machine, regardless of which interface the listener is
// bound to (e.g. -H 0.0.0.0 still serves a UI meant to be opened at
// localhost).
func (s *Server) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser client: CLI, script, curl, or a WS library that never
		// sets Origin. Browsers always send this header for cross-origin
		// (and same-origin) WS handshakes, so an empty value can't come from
		// a page in a browser.
		return true
	}

	allowed := []string{
		"http://localhost:" + s.port,
		"http://127.0.0.1:" + s.port,
		"http://[::1]:" + s.port,
	}
	for _, a := range allowed {
		if strings.EqualFold(origin, a) {
			return true
		}
	}
	return false
}

// New creates a Server. Pass a nil staticFS to proxy the dev server instead.
// Use addr "host:0" to let the OS pick a free port.
func New(a *appPkg.App, addr string, staticFS fs.FS) *Server {
	return &Server{
		app:         a,
		hub:         newHub(),
		auth:        newAuth(),
		addr:        addr,
		static:      staticFS,
		shutdownSig: make(chan struct{}),
	}
}

// Token returns the auth token to be printed in the terminal.
func (s *Server) Token() string { return s.auth.Token() }

// shutdownChan returns the shutdown signal channel, lazily creating it so
// Servers built as bare struct literals (tests) behave like New-built ones.
func (s *Server) shutdownChan() chan struct{} {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()
	if s.shutdownSig == nil {
		s.shutdownSig = make(chan struct{})
	}
	return s.shutdownSig
}

// shutdownSignal is the read-side accessor for the shutdown watcher in
// Start and for tests waiting on the signal.
func (s *Server) shutdownSignal() <-chan struct{} { return s.shutdownChan() }

// requestShutdown asks Start()'s watcher to run the graceful shutdown
// sequence. Idempotent: the channel is closed exactly once; later calls
// are no-ops. This is the ONLY thing the WS handler needs to do — the
// watcher then runs the same srv.Shutdown path a cancelled ctx takes.
func (s *Server) requestShutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdownChan()) })
}

// Start runs the server until ctx is cancelled. onReady is called once the
// listener is bound (with the actual address, useful when port was 0).
func (s *Server) Start(ctx context.Context, onReady func(addr string)) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return err
	}

	// Record the actual bound port (resolves "host:0" to whatever the OS
	// picked) so checkOrigin can validate the WS handshake's Origin header
	// against the port this server is really reachable on.
	if _, port, splitErr := net.SplitHostPort(ln.Addr().String()); splitErr == nil {
		s.port = port
	}

	go s.hub.Run(ctx)
	go subscribeAndBroadcast(ctx, s.app, s.hub)
	// Best-effort, once per start, in its own goroutine so a slow release
	// API never delays the listener becoming usable.
	go broadcastUpdateNotice(ctx, s.hub)

	mux := http.NewServeMux()

	// Auth endpoints ΓÇö no cookie required.
	mux.HandleFunc("/auth", s.auth.HandleAuth)
	mux.HandleFunc("/auth/check", s.auth.HandleAuthCheck)

	// WebSocket ΓÇö requires valid session cookie.
	mux.Handle("/ws", s.auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server-lifetime ctx (same one Hub.Run drains) is what the
		// guarded register send inside handleWS needs; r.Context() alone
		// cannot unblock it (it ends only when the handler returns).
		s.handleWS(ctx, w, r)
	})))

	if s.static != nil {
		// Serve the embedded React build; fall back to index.html for SPA routing.
		mux.Handle("/", spaHandler(s.static))
	} else {
		// Dev mode: proxy to the rspack dev server.
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://localhost:3000"+r.RequestURI, http.StatusTemporaryRedirect)
		})
	}

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming responses have no timeout
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		// ctx.Done() is the SIGINT/SIGTERM path; shutdownSignal is the
		// WS-triggered shutdown_server command (task #714). Both converge
		// here so the teardown sequence is identical either way.
		select {
		case <-ctx.Done():
		case <-s.shutdownSignal():
		}
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("rush web server listening", "addr", ln.Addr().String())

	// Notify caller synchronously ΓÇö address is known, server not yet serving.
	if onReady != nil {
		onReady(ln.Addr().String())
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// handleWS upgrades the connection and runs the client read/write pumps.
// ctx is the server's lifetime context (Start's), so the register send
// below can be abandoned when the hub has already stopped.
func (s *Server) handleWS(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		ReadBufferSize:  64 * 1024,
		WriteBufferSize: 64 * 1024,
		CheckOrigin:     s.checkOrigin,
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("ws: upgrade failed", "err", err)
		return
	}

	c := newClient(s.hub, conn)
	// register is buffered (64) but nothing drains it once Hub.Run has
	// returned, so a handshake racing server shutdown past that buffer
	// would park this goroutine — and the teardown only this function
	// performs — until process exit. Mirror readPump's guarded unregister
	// send below: abandon the client instead of blocking.
	select {
	case s.hub.register <- c:
	case <-ctx.Done():
		// Handshake lost the shutdown race. Tear the just-constructed
		// client down the way readPump's defers would have: close the
		// socket and stop newClient's worker pool. No unregister send —
		// the hub is gone, nothing would read it.
		_ = conn.Close()
		close(c.workQueue)
		return
	}

	// Start write pump in background; read pump blocks this goroutine.
	go c.writePump()
	s.readPump(r.Context(), c)
}

// readPump reads messages from the WebSocket and dispatches them.
func (s *Server) readPump(ctx context.Context, c *Client) {
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
		case s.hub.unregister <- c:
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
		handleIncoming(ctx, s, c, raw)
	}
}

// spaHandler serves static files and falls back to index.html for any path
// that doesn't match a real file (needed for client-side routing).
func spaHandler(fsys fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "index.html"
		}
		// Check if the file exists in the embedded FS.
		if _, err := fs.Stat(fsys, path[1:]); err == nil {
			fileServer.ServeHTTP(w, r)
			return
		}
		// Not found ΓÇö serve index.html so React Router can handle it.
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
