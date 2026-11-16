// Package server implements the HTTP + WebSocket server for rush's web mode.
// It serves the embedded React application and bridges the app's pubsub
// event system to connected browsers over WebSocket.
package server

import (
	"context"
	"crypto/tls"
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
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire/finalgate"
)

var (
	ErrUnexpectedNonLoopbackBind = errors.New("bound non-loopback address requires --allow-cleartext")
	Listen                       = func(ctx context.Context, network, address string) (net.Listener, error) {
		var lc net.ListenConfig
		return lc.Listen(ctx, network, address)
	}
)

type Server struct {
	app          *appPkg.App
	hub          *Hub
	auth         *Auth
	addr         string
	static       fs.FS
	policy       ListenerPolicy
	tlsStore     *tlsCertificateStore
	uiAuth       bool
	updateNotice func(context.Context, *Hub)

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
	connections  map[net.Conn]http.ConnState
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
	// A server built by NewWithPolicy carries explicit surfaces; a bare Server keeps the legacy rule.
	if !s.uiAuth && len(s.policy.Surfaces) > 0 {
		return OriginsAllowed(r.Header.Get("Origin"), s.policy.AllowedOrigins)
	}
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
	if err := ValidateAllowedOrigins(s.policy.AllowedOrigins); err != nil {
		return &ListenerPolicyError{Address: s.policy.Address, Rule: "invalid allowed origin", Cause: err}
	}
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
		auth = NewAuth()
	}
	return &Server{
		app:          a,
		hub:          NewHub(),
		auth:         auth,
		addr:         policy.Address,
		static:       staticFS,
		policy:       policy,
		uiAuth:       uiAuth,
		updateNotice: broadcastUpdateNotice,
		shutdownSig:  make(chan struct{}),
	}
}

func (s *Server) TLSPins() (TLSPins, error) {
	if s.tlsStore == nil {
		return TLSPins{}, nil
	}
	return s.tlsStore.Pins()
}

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
	if !IsLoopbackHost(listenerHost(ln.Addr())) && !s.policy.AllowCleartext && !s.policy.TLS.HasCertificatePair() {
		_ = ln.Close()
		return fmt.Errorf("%w: %q", ErrUnexpectedNonLoopbackBind, ln.Addr().String())
	}
	return s.ServeListener(ctx, ln, onReady)
}

// ServeListener serves on a listener that was prebound by the caller. The
// post-bind host check is repeated here for callers that inject a listener.
func (s *Server) ServeListener(ctx context.Context, ln net.Listener, onReady func(addr string)) error {
	if !IsLoopbackHost(listenerHost(ln.Addr())) && !s.policy.AllowCleartext && !s.policy.TLS.HasCertificatePair() {
		_ = ln.Close()
		return fmt.Errorf("%w: %q", ErrUnexpectedNonLoopbackBind, ln.Addr().String())
	}
	if !s.uiAuth {
		if err := s.validatePolicy(); err != nil {
			_ = ln.Close()
			return err
		}
	}
	var serverTLS *tls.Config
	var certificateStore *tlsCertificateStore
	if s.policy.TLS.Configured() {
		var err error
		certificateStore, err = newTLSCertificateStore(s.policy.TLS)
		if err != nil {
			_ = ln.Close()
			return err
		}
		serverTLS, err = tlsConfig(s.policy.TLS, certificateStore)
		if err != nil {
			_ = ln.Close()
			return err
		}
	}
	s.tlsStore = certificateStore

	// Record the actual bound port (resolves "host:0" to whatever the OS
	// picked) so checkOrigin can validate the WS handshake's Origin header
	// against the port this server is really reachable on.
	if _, port, splitErr := net.SplitHostPort(ln.Addr().String()); splitErr == nil {
		s.port = port
	}

	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	if certificateStore != nil {
		go runTLSReload(runCtx, certificateStore)
	}
	var joined sync.WaitGroup
	joined.Add(1)
	go func() {
		defer joined.Done()
		s.hub.Run(runCtx)
	}()
	if s.app != nil {
		forwarders := subscribeAndBroadcast(runCtx, s.app, s.hub, finalgate.GateFor(s.app))
		joined.Add(1)
		go func() {
			defer joined.Done()
			forwarders.Wait()
		}()
	}
	go s.updateNotice(runCtx, s.hub)

	mux := http.NewServeMux()
	if s.policyHasSurface(SurfaceWS) {
		// Sprint 0 L4 (S4, design §3): the gate is mounted directly — no
		// delegating method remains on Server. The subprotocol guard wraps the
		// gate exactly as it wrapped Server.handleWS at HEAD (bearer tokens
		// must not downgrade to a non-rush.v1 subprotocol on policy
		// listeners); the composer's OnIncoming closure keeps the dispatch
		// switch in package server until S5/L5.
		ws := hub.NewGate(hub.GateOptions{
			Hub:         s.hub,
			Ctx:         runCtx,
			CheckOrigin: s.checkOrigin,
			UIAuth:      s.uiAuth,
			AllowAdmin:  s.policy.AllowAdmin,
			OnIncoming:  func(ctx context.Context, c *Client, raw []byte) { handleIncoming(ctx, s, c, raw) },
			BeforeWrite: beforeClientWrite,
			Workers:     &s.wsWorkers,
		})
		if !s.uiAuth {
			gated := ws
			ws = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hasBearerSubprotocol(r) && !offeredSubprotocol(r, "rush.v1") {
					writeProblem(w, http.StatusBadRequest, "{\"type\":\"about:blank\",\"title\":\"Bad Request\",\"status\":400}\n")
					return
				}
				// The server-lifetime ctx (same one Hub.Run drains) is what the
				// guarded register send inside handleWS needs; r.Context() alone
				// cannot unblock it (it ends only when the handler returns).
				// GateOptions.Ctx carries it (see hub.GateOptions).
				gated.ServeHTTP(w, r)
			})
		}
		if s.uiAuth {
			ws = s.auth.Middleware(ws)
		} else if s.policy.Auth != nil {
			ws = s.policy.Auth(ws)
		}
		mux.Handle("/ws", ws)
	}
	if s.policyHasSurface(SurfaceREST) {
		rest := s.policy.RESTHandler
		if rest == nil {
			rest = http.NotFoundHandler()
			if s.policy.Auth != nil {
				rest = s.policy.Auth(rest)
			}
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
		ConnState:         s.trackConnection,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0, // streaming responses have no timeout
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		Protocols:         func() *http.Protocols { p := new(http.Protocols); p.SetHTTP1(true); return p }(),
		ErrorLog:          tlsErrorLogger(),
	}
	if serverTLS != nil {
		srv.TLSConfig = serverTLS
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
		s.closeQuiescentConnections()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.shutdownHTTP(shutCtx, srv)
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
	var serveErr error
	if serverTLS != nil {
		serveErr = srv.ServeTLS(ln, "", "")
	} else {
		serveErr = srv.Serve(ln)
	}
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
		s.connections = make(map[net.Conn]http.ConnState)
	}
	s.connections[conn] = state
}

func (s *Server) closeQuiescentConnections() {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	for conn, state := range s.connections {
		if state == http.StateNew || state == http.StateIdle {
			_ = conn.Close()
		}
	}
}

// shutdownHTTP runs srv.Shutdown while sweeping quiescent connections until it
// returns. net/http counts a StateNew connection as active for its first 5 s,
// so a connection that was accepted just before the listener closed but is
// registered (StateNew) only after the single sweep in Start (a client's
// speculative dial that never sends a request) would hold Shutdown for those 5 s.
func (s *Server) shutdownHTTP(ctx context.Context, srv *http.Server) error {
	done := make(chan error, 1)
	go func() { done <- srv.Shutdown(ctx) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			s.closeQuiescentConnections()
		}
	}
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
