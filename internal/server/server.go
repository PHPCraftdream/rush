// Package server implements the HTTP + WebSocket server for rush's web mode.
// It serves the embedded React application and bridges the app's pubsub
// event system to connected browsers over WebSocket.
package server

import (
	"context"
	"errors"
	"fmt"
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

var (
	ErrUnexpectedNonLoopbackBind = errors.New("bound non-loopback address requires --allow-cleartext")
	Listen                       = func(ctx context.Context, network, address string) (net.Listener, error) {
		var lc net.ListenConfig
		return lc.Listen(ctx, network, address)
	}
)

type Server struct {
	app    *appPkg.App
	hub    *Hub
	auth   *Auth
	addr   string
	static fs.FS
	policy ListenerPolicy
	uiAuth bool

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
	connMu       sync.Mutex
	connections  map[net.Conn]struct{}
	wsWorkers    sync.WaitGroup
}

func (s *Server) policyHasSurface(surface ListenerSurface) bool {
	for _, configured := range s.policy.Surfaces {
		if configured == surface {
			return true
		}
	}
	return false
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

func (s *Server) validatePolicy() error {
	for _, surface := range s.policy.Surfaces {
		if surface == SurfaceUI {
			return &ListenerPolicyError{Address: s.policy.Address, Rule: "UI surface is not permitted for serve policies"}
		}
	}
	return s.policy.Validate()
}

// New creates a Server. Pass a nil staticFS to proxy the dev server instead.
// Use addr "host:0" to let the OS pick a free port.
func New(a *appPkg.App, addr string, staticFS fs.FS) *Server {
	return newServer(a, ListenerPolicy{
		Address:  addr,
		Surfaces: []ListenerSurface{SurfaceWS, SurfaceUI},
	}, staticFS, true)
}

// NewWithCleartext creates a UI server with an explicit non-loopback bind opt-in.
func NewWithCleartext(a *appPkg.App, addr string, staticFS fs.FS, allowCleartext bool) *Server {
	s := New(a, addr, staticFS)
	s.policy.AllowCleartext = allowCleartext
	return s
}

// NewWithPolicy creates a Server whose listener surfaces and authentication are
// determined by policy. Unlike New, it never creates a UI token implicitly.
func NewWithPolicy(a *appPkg.App, policy ListenerPolicy, staticFS fs.FS) *Server {
	policy = cloneListenerPolicy(policy)
	if policy.Auth == nil {
		policy.Auth = DenyAllAuth
	}
	return newServer(a, policy, staticFS, false)
}

func cloneListenerPolicy(policy ListenerPolicy) ListenerPolicy {
	policy.Surfaces = append([]ListenerSurface(nil), policy.Surfaces...)
	policy.AllowedOrigins = append([]string(nil), policy.AllowedOrigins...)
	policy.TokenIDs = append([]string(nil), policy.TokenIDs...)
	return policy
}

func newServer(a *appPkg.App, policy ListenerPolicy, staticFS fs.FS, uiAuth bool) *Server {
	policy = cloneListenerPolicy(policy)
	var auth *Auth
	if uiAuth {
		auth = newAuth()
	}
	return &Server{
		app:         a,
		hub:         newHub(),
		auth:        auth,
		addr:        policy.Address,
		static:      staticFS,
		policy:      policy,
		uiAuth:      uiAuth,
		shutdownSig: make(chan struct{}),
	}
}

// Token returns the auth token to be printed in the terminal.
func (s *Server) Token() string {
	if s.auth == nil {
		return ""
	}
	return s.auth.Token()
}

func (s *Server) RequestShutdown() { s.requestShutdown() }

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
	addr := s.addr
	if !s.uiAuth {
		var err error
		addr, err = s.policy.ResolvedAddress()
		if err != nil {
			return err
		}
		validatedPolicy := s.policy
		validatedPolicy.Address = addr
		if err := validatedPolicy.Validate(); err != nil {
			return err
		}
		s.policy.Address = addr
	}
	ln, err := Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if !IsLoopbackHost(listenerHost(ln.Addr())) && !s.policy.AllowCleartext {
		_ = ln.Close()
		return fmt.Errorf("%w: %q", ErrUnexpectedNonLoopbackBind, ln.Addr().String())
	}
	return s.ServeListener(ctx, ln, onReady)
}

// ServeListener serves on a listener that was prebound by the caller. The
// post-bind host check is repeated here for callers that inject a listener.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener, onReady func(addr string)) error {
	if !IsLoopbackHost(listenerHost(ln.Addr())) && !s.policy.AllowCleartext {
		_ = ln.Close()
		return fmt.Errorf("%w: %q", ErrUnexpectedNonLoopbackBind, ln.Addr().String())
	}
	if !s.uiAuth {
		if err := s.validatePolicy(); err != nil {
			_ = ln.Close()
			return err
		}
	}

	// Record the actual bound port (resolves "host:0" to whatever the OS
	// picked) so checkOrigin can validate the WS handshake's Origin header
	// against the port this server is really reachable on.
	if _, port, splitErr := net.SplitHostPort(ln.Addr().String()); splitErr == nil {
		s.port = port
	}

	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	var joined sync.WaitGroup
	joined.Add(1)
	go func() {
		defer joined.Done()
		s.hub.Run(runCtx)
	}()
	if s.app != nil {
		forwarders := subscribeAndBroadcast(runCtx, s.app, s.hub)
		joined.Add(1)
		go func() {
			defer joined.Done()
			forwarders.Wait()
		}()
	}
	joined.Add(1)
	go func() {
		defer joined.Done()
		broadcastUpdateNotice(runCtx, s.hub)
	}()

	mux := http.NewServeMux()
	if s.policyHasSurface(SurfaceWS) {
		var ws http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The server-lifetime ctx (same one Hub.Run drains) is what the
			// guarded register send inside handleWS needs; r.Context() alone
			// cannot unblock it (it ends only when the handler returns).
			s.handleWS(runCtx, w, r)
		})
		if s.uiAuth {
			ws = s.auth.Middleware(ws)
		} else if s.policy.Auth != nil {
			ws = s.policy.Auth(ws)
		}
		mux.Handle("/ws", ws)
	}
	if s.policyHasSurface(SurfaceREST) {
		rest := http.Handler(http.NotFoundHandler())
		if s.policy.Auth != nil {
			rest = s.policy.Auth(rest)
		}
		mux.Handle("/api/v1/", rest)
	}
	if s.uiAuth && s.auth != nil {
		mux.HandleFunc("/auth", s.auth.HandleAuth)
		mux.HandleFunc("/auth/check", s.auth.HandleAuthCheck)
	}
	if s.policyHasSurface(SurfaceUI) {
		if s.static != nil {
			mux.Handle("/", spaHandler(s.static))
		} else {
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "http://localhost:3000"+r.RequestURI, http.StatusTemporaryRedirect)
			})
		}
	}

	var handlers sync.WaitGroup
	var admissionMu sync.Mutex
	admissionClosed := false
	trackedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		admissionMu.Lock()
		if admissionClosed {
			admissionMu.Unlock()
			http.Error(w, "server is shutting down", http.StatusServiceUnavailable)
			return
		}
		handlers.Add(1)
		admissionMu.Unlock()
		defer handlers.Done()
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{
		Handler: trackedHandler,
		BaseContext: func(net.Listener) context.Context {
			return runCtx
		},
		ConnState:    s.trackConnection,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming responses have no timeout
		IdleTimeout:  120 * time.Second,
		ErrorLog:     slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}
	if !s.uiAuth {
		srv.ReadHeaderTimeout = 10 * time.Second
	}

	shutdownDone := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
		case <-s.shutdownSignal():
		}
		// Stop admission before any cancellation or wait begins. The mutex
		// makes handler WaitGroup.Add impossible after the shutdown waiter starts.
		_ = ln.Close()
		admissionMu.Lock()
		admissionClosed = true
		admissionMu.Unlock()
		cancelRun()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := srv.Shutdown(shutCtx)
		cancel()
		if errors.Is(err, net.ErrClosed) {
			err = nil
		}
		if err != nil {
			_ = srv.Close()
			s.closeConnections()
		}
		cancelRun()
		s.closeConnections()
		joinCtx, cancelJoin := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelJoin()
		joinDone := make(chan struct{})
		go func() {
			joined.Wait()
			handlers.Wait()
			s.wsWorkers.Wait()
			close(joinDone)
		}()
		select {
		case <-joinDone:
		case <-joinCtx.Done():
			err = errors.Join(err, joinCtx.Err())
		}
		shutdownDone <- err
	}()

	slog.Info("rush web server listening", "addr", ln.Addr().String())

	// Notify caller synchronously ΓÇö address is known, server not yet serving.
	if onReady != nil {
		onReady(ln.Addr().String())
	}
	serveErr := srv.Serve(ln)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		s.requestShutdown()
	}
	shutdownErr := <-shutdownDone
	if serveErr != nil && serveErr != http.ErrServerClosed && !errors.Is(serveErr, net.ErrClosed) {
		return serveErr
	}
	return shutdownErr
}

func (s *Server) trackConnection(conn net.Conn, state http.ConnState) {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if state == http.StateClosed {
		delete(s.connections, conn)
		return
	}
	if s.connections == nil {
		s.connections = make(map[net.Conn]struct{})
	}
	s.connections[conn] = struct{}{}
}

func (s *Server) closeConnections() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for conn := range s.connections {
		_ = conn.Close()
	}
}

func listenerHost(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return ""
	}
	return host
}

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
	s.wsWorkers.Add(1)
	go func() {
		defer s.wsWorkers.Done()
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
	if !s.hub.tryRegister(ctx, c) {
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
		case <-s.hub.stopped:
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
