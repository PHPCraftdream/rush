// apiauth_bridge.go: temporary compatibility surface for package server after
// the S2 seam moved the api-authentication leaf to internal/serve/server/apiauth
// (sprint 0 lane L2). Design lanes L6 (rest) and L7 (composer) are the
// deleters: as their consumers move out of package server, the aliases below
// disappear with them. Type aliases and const re-exports only — no mutable
// var re-exports (the two deleted S2 var fallbacks must not reappear in any
// form).
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
)

type (
	// Core S2 surface (design §2.2).
	Principal     = apiauth.Principal
	ChainConfig   = apiauth.ChainConfig
	AuthConfig    = apiauth.AuthConfig
	TokenVerifier = apiauth.TokenVerifier
	TokenFlusher  = apiauth.TokenFlusher
	Ticker        = apiauth.Ticker
	Pagination    = apiauth.Pagination
	// APIProblem is already exported at HEAD (api_response.go:18); design §2.2
	// omits it, but api_run_handlers.go uses it (existing export contract).
	APIProblem = apiauth.APIProblem
	// Auth is already exported at HEAD (auth.go:21); design §2.2 omits it, but
	// server.go:36 and api_serve.go keep a *Auth field until L7 (existing
	// export contract).
	Auth = apiauth.Auth
	// RateLimiter is the designed port (S2, L2) for the health-endpoint limiter
	// built by api_serve.go (was unexported rateLimiter/newRateLimiter).
	RateLimiter = apiauth.RateLimiter
)

const (
	// Listener route values (design §2.4 row 3: declared in apiauth).
	RouteFull  = apiauth.RouteFull
	RouteBatch = apiauth.RouteBatch

	// UI session cookie name (auth.go:12 at HEAD, now unexported in apiauth);
	// local copy of the literal for server tests until L7.
	cookieName = "rush_session"
)

// Function re-exports: one-line delegations so ~150 package-server files keep
// their unqualified call sites until lanes L6/L7 move them out. Pure pass-
// through wrappers — no state, no fallbacks, no mutable vars.

func Redact(s string) string { return apiauth.Redact(s) }

func RedactURL(u *url.URL) *url.URL { return apiauth.RedactURL(u) }

func RedactHeader(h http.Header) http.Header { return apiauth.RedactHeader(h) }

func RecordScope(ctx context.Context, scope string) { apiauth.RecordScope(ctx, scope) }

func RecordSession(ctx context.Context, id string) { apiauth.RecordSession(ctx, id) }

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return apiauth.WithPrincipal(ctx, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) { return apiauth.PrincipalFrom(ctx) }

func WriteAPIResponse(w http.ResponseWriter, r *http.Request, status int, payload any) {
	apiauth.WriteAPIResponse(w, r, status, payload)
}

func WriteAPIProblem(w http.ResponseWriter, r *http.Request, status int, title, detail, instance string, cause error) {
	apiauth.WriteAPIProblem(w, r, status, title, detail, instance, cause)
}

func ParsePagination(r *http.Request) (Pagination, error) { return apiauth.ParsePagination(r) }

func WritePaginatedResponse(w http.ResponseWriter, r *http.Request, status int, items any, ids []string, hasMore bool) {
	apiauth.WritePaginatedResponse(w, r, status, items, ids, hasMore)
}

func Chain(cfg ChainConfig) func(http.Handler) http.Handler { return apiauth.Chain(cfg) }

func StillValid(p Principal, now time.Time) bool { return apiauth.StillValid(p, now) }

func RunUsageFlusher(ctx context.Context, f TokenFlusher, every time.Duration, factories ...func(time.Duration) Ticker) {
	apiauth.RunUsageFlusher(ctx, f, every, factories...)
}

func FingerprintAuth(next http.Handler) http.Handler { return apiauth.FingerprintAuth(next) }

func NewRateLimiter(rps float64, burst int, clock func() time.Time) *RateLimiter {
	return apiauth.NewRateLimiter(rps, burst, clock)
}

func RealTicker(d time.Duration) Ticker { return apiauth.RealTicker(d) }

func NewVerifiablePrincipal(tokenID string, verifier TokenVerifier, tickerFactory func(time.Duration) Ticker) Principal {
	return apiauth.NewVerifiablePrincipal(tokenID, verifier, tickerFactory)
}

func NewAuth() *Auth { return apiauth.NewAuth() }

func ResolvePinnedCommand(pin, command string, raw json.RawMessage) int {
	return apiauth.ResolvePinnedCommand(pin, command, raw)
}

func ResolveBatchOnlyCommand(command string) int { return apiauth.ResolveBatchOnlyCommand(command) }

func ResolvePinnedRoute(pin, operationID, pathID string) int {
	return apiauth.ResolvePinnedRoute(pin, operationID, pathID)
}

func ResolveBatchOnlyRoute(operationID string) int { return apiauth.ResolveBatchOnlyRoute(operationID) }

func RouteClassIsKnown(operationID string) bool { return apiauth.RouteClassIsKnown(operationID) }

func SessionResolutionFor(command string) (field, class string, ok bool) {
	return apiauth.SessionResolutionFor(command)
}

func SessionResolutionCommands() []string { return apiauth.SessionResolutionCommands() }

// Small local copies (S2, L2): unexported helpers that stayed reachable from
// package-server production/test code while their definitions moved to
// apiauth. Each is a verbatim copy of the HEAD body (design §6 risk 3 escape:
// "small local copy"); lanes L6/L7 delete them with their callers.

// requestIDPattern (api_response.go:15 at HEAD).
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// setAPIResponseHeaders (api_response.go:53-78 at HEAD); callers:
// api_mux.go:49, api_run_handlers.go:58, api_serve.go:90.
func setAPIResponseHeaders(w http.ResponseWriter, r *http.Request) {
	id := w.Header().Get("X-Request-Id")
	if !requestIDPattern.MatchString(id) {
		id = ""
		if r != nil {
			id = r.Header.Get("X-Request-Id")
		}
		if !requestIDPattern.MatchString(id) {
			var bytes [16]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				id = strings.Repeat("0", 32)
			} else {
				id = hex.EncodeToString(bytes[:])
			}
		}
	}
	w.Header().Set("X-Request-Id", id)
	w.Header().Set("Cache-Control", "no-store")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
}

// sourceIP (ratelimit_core.go:8-13 at HEAD); caller api_serve.go:78.
func sourceIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err == nil {
		return host
	}
	return remote
}

// fmtInt + retryResponse (apiauth_chain.go at HEAD); caller api_serve.go:79.
func fmtInt(i int) string { return strconv.Itoa(i) }

func retryResponse(w http.ResponseWriter, d time.Duration) {
	if d < time.Second {
		d = time.Second
	}
	w.Header().Set("Retry-After", fmtInt(int((d+time.Second-1)/time.Second)))
	writeProblem(w, 429, authBusyBody)
}

// Auth failure/problem body copies (apiauth_chain.go:105-111 at HEAD) for
// retryResponse above and server tests asserting on the fixed bodies
// (api_serve_integration_test.go, api_serve_test.go).
const (
	authFailureBody     = "{\"type\":\"about:blank\",\"title\":\"Unauthorized\",\"status\":401}\n"
	authUnavailableBody = "{\"type\":\"about:blank\",\"title\":\"Service Unavailable\",\"status\":503}\n"
	tooLargeBody        = "{\"type\":\"about:blank\",\"title\":\"Request Too Large\",\"status\":413}\n"
	authBusyBody        = "{\"type\":\"about:blank\",\"title\":\"Too Many Requests\",\"status\":429}\n"
)

// writeProblem (apiauth_chain.go at HEAD); caller server.go:309.
func writeProblem(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// websocketUpgrade / hasBearerSubprotocol / offeredSubprotocol (ws_serve_auth.go
// at HEAD); callers server.go:308 and ws_serve_origin_test.go.
func websocketUpgrade(r *http.Request) bool {
	return websocket.IsWebSocketUpgrade(r)
}

func hasBearerSubprotocol(r *http.Request) bool {
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, item := range strings.Split(value, ",") {
			if strings.HasPrefix(strings.TrimSpace(item), "rush.bearer.") {
				return true
			}
		}
	}
	return false
}

func offeredSubprotocol(r *http.Request, protocol string) bool {
	for _, value := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, item := range strings.Split(value, ",") {
			if strings.TrimSpace(item) == protocol {
				return true
			}
		}
	}
	return false
}
