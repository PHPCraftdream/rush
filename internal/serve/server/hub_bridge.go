package server

// hub_bridge.go: temporary compatibility surface for package server after the
// S4 seam moved the WS hub to internal/serve/server/hub (sprint 0 split,
// design docs/plans/2026-11-15-sprint0-split-server.md §2.2/§3 S4). Deleter
// lanes: L5 (ws families migrate to hub.Client methods / hub.Hub accessors)
// and L7 (composer + remaining fixture tests). Type aliases and one-line
// delegating wrappers only — no mutable var re-exports.
//
// Trimmed in L4: none. The S4 candidates were ResolvePinnedCommand and
// ResolveBatchOnlyCommand (their production consumer, the handlers.go
// authorization block, moved into hub.Client.AuthorizeIncoming), but the
// staying white tests api_session_resolution_test.go:51 and
// api_batch_only_test.go:151-158 still use them, so the apiauth_bridge
// entries remain (L6/L7 delete them with their tests). StillValid/RealTicker
// keep their production consumer api_events_revoke.go:11,18. wire_bridge.go
// and listener_bridge.go: all entries remain (the moved hub files now
// qualify wire/apiauth directly, but every moved name still has
// package-server consumers).

import (
	"context"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

type (
	// Core S4 surface (design §2.2).
	Hub    = hub.Hub
	Client = hub.Client
	// titleCache is the §2.2 TitleCache under its HEAD name: the live-work
	// files that stay in package server until L5 (handlers_livework.go:65,
	// handlers_livework_batch.go:72) keep their unqualified parameter types.
	titleCache = hub.TitleCache
)

var (
	testHookMu      sync.RWMutex
	testBeforeWrite func([]byte)
)

func beforeClientWrite(msg []byte) {
	testHookMu.RLock()
	hook := testBeforeWrite
	testHookMu.RUnlock()
	if hook != nil {
		hook(msg)
	}
}

// NewHub wraps hub.NewHub for the ~30 package-server tests/fixtures that call
// the unqualified constructor until L5/L7.
func NewHub() *Hub { return hub.NewHub() }

// newClient mirrors the HEAD unqualified constructor used by server-side test
// fixtures (readpump_control_starvation_test.go dialTestWS,
// handlers_shutdown_test.go, ~30 more) until L5/L7.
func newClient(h *Hub, conn *websocket.Conn) *Client { return hub.NewClient(h, conn) }

// newTitleCache mirrors the HEAD unqualified constructor (handlers_livework_*
// tests) until L5.
func newTitleCache() *titleCache { return hub.NewTitleCache() }

// newClientAuth mirrors the HEAD unqualified constructor; two staying tests
// assign auth through Client.SetAuth (handlers_config_lock_trusted_test.go:575,
// handlers_custom_roles_test.go:95) until L5.
func newClientAuth(principal Principal, policy hub.CommandPolicy) *hub.ClientAuth {
	return hub.NewClientAuth(principal, policy)
}

// filterSessionsForPinnedClient re-exports the §2.2 rename for its staying
// consumer handlers_sessions.go:332 until L5.
func filterSessionsForPinnedClient(sessions []session.Session, c *Client) []session.Session {
	return hub.FilterSessionsForPinnedClient(sessions, c)
}

// pinnedSessionsOnly re-exports the exported method for its staying root
// consumer api_read_handlers.go:123 until L6.
func pinnedSessionsOnly(sessions []session.Session, pin string) []session.Session {
	return hub.PinnedSessionsOnly(sessions, pin)
}

// ServeWSGate builds the S4 WS gateway for the server-side test fixture that
// mounts the same chain the composer builds
// (readpump_control_starvation_test.go dialTestWS until L7). Not a delegating
// method on Server: the composer (server.go) qualifies hub.NewGate directly.
func ServeWSGate(s *Server, ctx context.Context) http.Handler {
	return hub.NewGate(hub.GateOptions{
		Hub:         s.hub,
		Ctx:         ctx,
		CheckOrigin: s.checkOrigin,
		UIAuth:      s.uiAuth,
		AllowAdmin:  s.policy.AllowAdmin,
		OnIncoming:  func(ctx context.Context, c *Client, raw []byte) { handleIncoming(ctx, s, c, raw) },
		BeforeWrite: beforeClientWrite,
		Workers:     &s.wsWorkers,
	})
}

// Re-exported production sizes for root call sites; see
// hub.WorkerPoolSize/MaxMessageSize.
const (
	workerPoolSize = hub.WorkerPoolSize
	maxMessageSize = hub.MaxMessageSize
)

// Re-exported ring capacity for root call sites; see hub.MaxBufferSize.
const maxBufferSize = hub.MaxBufferSize

// commandScopes re-exports the moved scope table for its staying white-test
// consumers (api_session_resolution_test.go) until L6/L7; snapshot copy — no
// mutable var crosses the boundary.
var commandScopes = hub.CommandScopesSnapshot()

// newHub is the HEAD unqualified constructor used by ~30 staying tests
// (descendant_work_*, p-family, handlers-family fixtures) until L5/L7.
func newHub() *Hub { return hub.NewHub() }

// newDetachedTestClient builds a Client detached from any real connection
// with an n-slot send channel for root tests that wrote
// &Client{send: make(chan []byte, n)} struct literals before the hub split
// (events_config_test.go:74, p569_*, p595_* at HEAD). Provenance:
// p460_model_scope_test.go newTestClient pattern. L5/L7 remove the need.
func newDetachedTestClient(sendBuf int) *Client {
	c := hub.NewDetachedClient(hub.NewHub(), sendBuf)
	return c
}

type (
	// CommandPolicy and ClientAuth re-export the moved scope-table types for
	// their staying test consumers (handlers_config_lock_trusted_test.go:575,
	// handlers_custom_roles_test.go:95) until L5.
	CommandPolicy = hub.CommandPolicy
	ClientAuth    = hub.ClientAuth
)

// Scope-table function re-exports (design §2.2 renames) for their staying
// test consumers (handlers_custom_roles_test.go:110-114,
// peak_hours_wire_v2_test.go:241-251, ws_serve_scope_matrix_test.go) until
// L5/L7.

func AuthorizeCommand(p Principal, cmd string, policy CommandPolicy) error {
	return hub.AuthorizeCommand(p, cmd, policy)
}

func ScopeFor(cmd string) (string, bool) { return hub.ScopeFor(cmd) }

var (
	ErrScopeForbidden  = hub.ErrScopeForbidden
	ErrAdminDisallowed = hub.ErrAdminDisallowed
	ErrCommandUnmapped = hub.ErrCommandUnmapped
)

// startServeRevocationWatch re-exports the moved WS revocation watcher
// (ws_serve_revoke.go, design §2.4 row 81) for its staying white test
// ws_serve_revoke_test.go until L7.
func startServeRevocationWatch(ctx context.Context, conn *websocket.Conn, principal Principal) {
	hub.StartServeRevocationWatch(ctx, conn, principal)
}
