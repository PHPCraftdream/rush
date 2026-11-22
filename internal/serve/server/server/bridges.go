package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
	"github.com/PHPCraftdream/rush/internal/serve/server/listener"
	"github.com/PHPCraftdream/rush/internal/serve/server/rest/api"
	"github.com/PHPCraftdream/rush/internal/serve/server/rest/apibatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/gorilla/websocket"
)

// api_bridge.go: temporary compatibility surface for package server after the
// REST api family moved to internal/serve/server/rest/api and
// internal/serve/server/rest/apibatch (sprint 0 split, design
// docs/plans/2026-11-15-sprint0-split-server.md). Same technique as
// wire_bridge.go / apiauth_bridge.go / hub_bridge.go / listener_bridge.go:
// type aliases, const re-exports and one-line delegating wrappers, each entry
// keeping the HEAD name so the root package's producers and tests compile
// unchanged. Deleter lane: L7 (server composer) removes the last name once the
// root tests and the composer move to the new packages. The one var below is a
// pointer view, not a copy, because a struct-field selector cannot be aliased.

type (
	// APIRoute is the route declaration (owner: rest/api/routes.go); callers
	// api_serve.go:25, api_mux_test.go:41.
	APIRoute = api.APIRoute
	// APIReadStore is the read seam (owner: rest/api/read.go); caller
	// api_read_errors_test.go:105.
	APIReadStore = api.APIReadStore
	// APIWriteStore is the session-write seam (owner: rest/api/read.go);
	// callers api_run_handlers_test.go:127, api_session_handlers_test.go:54.
	APIWriteStore = api.APIWriteStore
	// apiAppWriteStore is the production write store (owner:
	// rest/api/read.go); callers api_session_lock_test.go:15,
	// finalization_wiring_test.go:364.
	apiAppWriteStore = api.APIAppWriteStore
	// APIEventLog is the per-session event log (owner: rest/api/events.go);
	// callers api_serve.go:25, api_events_behavior_test.go:48.
	APIEventLog = api.APIEventLog
	// APIRun is one API run record (owner: rest/api/runhandlers.go); callers
	// api_events_test.go:127, api_runs_test.go:74.
	APIRun = api.APIRun
	// APIRunRegistry is the run registry (owner: rest/api/runhandlers.go);
	// callers api_serve.go:25, api_runs_test.go:68.
	APIRunRegistry = api.APIRunRegistry
	// APIRunBackend adapts the app runner (owner: rest/api/runbackend.go);
	// callers api_run_handlers_test.go:127, api_run_backend_test.go:220.
	APIRunBackend = api.APIRunBackend
	// appAPIRunBackend is the production backend (owner:
	// rest/api/runbackend.go); caller task76_api_run_backend_test.go:110.
	appAPIRunBackend = api.AppAPIRunBackend
	// RunOutcome is the backend's run result (owner: rest/api/runbackend.go);
	// callers api_run_backend_test.go:202, api_run_handlers_test.go:62.
	RunOutcome = api.RunOutcome
	// apiRunSelection is the pinned configuration/contract of one run (owner:
	// rest/api/runhandlers.go); callers task76_api_run_backend_test.go:119,
	// api_run_custom_role_admission_test.go:16.
	apiRunSelection = api.APIRunSelection
	// APIPermissionRequest is the public permission view (owner:
	// rest/api/permissions.go); caller api_permissions_integration_test.go:185.
	APIPermissionRequest = api.APIPermissionRequest
	// apiPermissionEvent is the broker emitter payload (owner:
	// rest/api/permissions.go); callers api_permissions_test.go:17,
	// api_permissions_handlers_test.go:32.
	apiPermissionEvent = api.APIPermissionEvent
	// apiPermissionTimer is the injectable expiry timer (owner:
	// rest/api/permissions.go); callers api_permissions_test.go:79,
	// api_permissions_handlers_test.go:33.
	apiPermissionTimer = api.APIPermissionTimer
	// APIPermissionBroker is the permission broker (owner:
	// rest/api/permissions.go); callers api_serve.go:25,43.
	APIPermissionBroker = api.APIPermissionBroker
	// idempotencyStore is the idempotency cache (owner:
	// rest/api/permissions.go); callers api_idempotency_test.go:17,
	// api_idempotency_expiry_test.go:14.
	idempotencyStore = api.IdempotencyStore
	// apiTrackingWriter is the response writer wrapper (owner:
	// rest/api/routes.go); callers api_serve.go:103,
	// api_events_wire_test.go:18.
	apiTrackingWriter = api.APITrackingWriter
	// apiEventFeeder is the background feeder (owner: rest/api/events.go);
	// caller api_serve.go:113.
	apiEventFeeder = api.APIEventFeeder
	// apiSSELimiter is the per-token SSE stream limiter (owner:
	// rest/api/events.go); callers api_events_handler_test.go:61,
	// api_events_behavior_test.go:160.
	apiSSELimiter = api.APISSELimiter
	// apiSSETicker is the injectable SSE ticker (owner: rest/api/events.go);
	// caller api_events_handler_test.go:87.
	apiSSETicker = api.APISSETicker
	// apiBatch is the batch DTO (owner: rest/apibatch/handlers.go); callers
	// api_batch_fixture_test.go:209, api_batch_fixture_diag_test.go:117.
	apiBatch = apibatch.APIBatch
	// apiBatchNode is the batch node DTO (owner: rest/apibatch/handlers.go);
	// callers api_batch_question_test.go:71, api_batch_intake_test.go:23.
	apiBatchNode = apibatch.APIBatchNode
	// apiBatchAccepted is the batch submission DTO (owner:
	// rest/apibatch/handlers.go); caller api_batch_fixture_test.go:200.
	apiBatchAccepted = apibatch.APIBatchAccepted
	// apiBatchService runs one App's batch intake (owner:
	// rest/apibatch/service.go); caller api_batch_fixture_test.go:35.
	apiBatchService = apibatch.APIBatchService
	// batchIntakePolicy is what one token may submit (owner:
	// rest/apibatch/policy.go); callers api_batch_policy_test.go:25,40.
	batchIntakePolicy = apibatch.BatchIntakePolicy
)

const (
	// SSE limits (owner: rest/api/events.go); callers
	// api_events_test.go:45, api_events_behavior_test.go:162.
	apiEventMaxSessions   = api.APIEventMaxSessions
	apiEventSubscriberCap = api.APIEventSubscriberCap
	apiSSEStreamsPerToken = api.APISSEStreamsPerToken

	// Run registry limits (owner: rest/api/runhandlers.go); callers
	// api_runs_test.go:69, api_run_handlers_test.go:216.
	apiRunMaxFieldBytes = api.ApiRunMaxFieldBytes
	apiRunMaxRetained   = api.APIRunMaxRetained
	apiRunTTL           = api.APIRunTTL

	// Idempotency outcomes and limits (owner: rest/api/permissions.go);
	// callers api_idempotency_test.go:18, api_idempotency_expiry_test.go:15.
	idempotencyMaxEntries = api.IdempotencyMaxEntries
	idempotencyTTL        = api.IdempotencyTTL
	idempotencyStarted    = api.IdempotencyStarted
	idempotencyReplay     = api.IdempotencyReplay
	idempotencyInProgress = api.IdempotencyInProgress
	idempotencyConflict   = api.IdempotencyConflict
	idempotencyFull       = api.IdempotencyFull

	// Permission limits (owner: rest/api/permissions.go); callers
	// api_permissions_gaps_test.go:107,120.
	maxPendingPermissionsPerSession = api.MaxPendingPermissionsPerSession
	permissionDescriptionLimit      = api.PermissionDescriptionLimit

	// Batch intake limits (owner: rest/apibatch/policy.go); callers
	// api_batch_policy_test.go:97,102.
	batchIntakeMaxNodes       = apibatch.BatchIntakeMaxNodes
	batchIntakeDefaultTimeout = apibatch.BatchIntakeDefaultTimeout
)

var (
	// Error sentinels (owner: rest/api/runbackend.go, rest/api/runhandlers.go
	// and rest/api/permissions.go); callers api_run_start_errors_test.go:23-26,
	// api_run_backend_test.go:35, api_run_custom_role_admission_test.go:48,
	// api_runs_test.go:98, api_permissions_gaps_test.go:71.
	ErrAPIRunBackendBusy            = api.ErrAPIRunBackendBusy
	ErrAPIRunBackendGateUnavailable = api.ErrAPIRunBackendGateUnavailable
	ErrAPIRunBackendInvalidModel    = api.ErrAPIRunBackendInvalidModel
	ErrAPIRunBackendInvalidRole     = api.ErrAPIRunBackendInvalidRole
	ErrAPIRunAutoApproveDenied      = api.ErrAPIRunAutoApproveDenied
	ErrAPIRunNotFound               = api.ErrAPIRunNotFound
	ErrAPIRunRegistryFull           = api.ErrAPIRunRegistryFull
	ErrAPIPermissionUnknown         = api.ErrAPIPermissionUnknown
	ErrAPIPermissionDecided         = api.ErrAPIPermissionDecided
	ErrAPIPermissionDecision        = api.ErrAPIPermissionDecision
	// apiBatchServices is the retained-service registry the fixture inspects
	// under its lock (api_batch_fixture_test.go:137-139). A pointer view, not a
	// copy: the mutex and the map are the package's own.
	apiBatchServices = &apibatch.APIBatchServices
)

// Route construction (owner: rest/api/read.go, routes.go, permissions.go).

// APIRoutesWithApp binds the read routes to the production App services;
// ported here from the old api_read_handlers.go. Callers
// api_read_handlers_test.go:199,230.
func APIRoutesWithApp(a *appPkg.App) []APIRoute {
	routes, _, _ := api.APIRoutesWithBroker(a, nil, false, apibatch.WiredRoutes)
	return routes
}

// APIRoutes is the declarative route table; caller api_feature_test.go:196.
func APIRoutes() []APIRoute { return api.APIRoutes() }

// APIRoutesWithDeps binds the read routes to a store; caller
// api_read_contract_test.go:18.
func APIRoutesWithDeps(deps APIReadStore) []APIRoute { return api.APIRoutesWithDeps(deps) }

// apiPermissionRoutes binds the permission routes to a broker; callers
// api_permissions_handlers_test.go:47, api_permissions_contract_test.go:42.
func apiPermissionRoutes(broker *APIPermissionBroker) []APIRoute {
	return api.APIPermissionRoutes(broker)
}

// apiRunWriteRoutes binds the run routes; caller api_run_handlers_test.go:128.
func apiRunWriteRoutes(store APIWriteStore, backend APIRunBackend) []APIRoute {
	return api.APIRunWriteRoutes(store, backend)
}

// apiSessionWriteRoutes binds the session routes; caller
// api_session_handlers_test.go:54.
func apiSessionWriteRoutes(store APIWriteStore) []APIRoute {
	return api.APISessionWriteRoutes(store)
}

// BuildOpenAPIDocument renders the OpenAPI document; callers
// api_openapi_docs_golden_test.go:17, api_openapi_feature_test.go:19.
func BuildOpenAPIDocument(routes []APIRoute) map[string]any {
	return api.BuildOpenAPIDocument(routes)
}

// NewAPIHandler muxes the routes; caller api_feature_test.go:23.
func NewAPIHandler(routes []APIRoute) http.Handler { return api.NewAPIHandler(routes) }

// NewAPIHandlerSplit splits public and protected routes; caller
// api_serve.go:81.
func NewAPIHandlerSplit(routes []APIRoute) (public http.Handler, protected http.Handler) {
	return api.NewAPIHandlerSplit(routes)
}

// cursorStart resolves a cursor offset; caller api_read_handlers_test.go:298.
func cursorStart[T any](items []T, after string, id func(T) string) (int, bool) {
	return api.CursorStart(items, after, id)
}

// lowercaseASCII lowercases ASCII letters; caller api_openapi_test.go:48.
func lowercaseASCII(value string) string { return api.LowercaseASCII(value) }

// Run registry and backend (owner: rest/api/runhandlers.go, runbackend.go).

// NewAPIRunRegistry builds a run registry; caller api_events_test.go:126.
func NewAPIRunRegistry(now func() time.Time) *APIRunRegistry {
	return api.NewAPIRunRegistry(now)
}

// newAPIRunBackend builds the production run backend; callers
// api_run_backend_test.go:220, task76_api_run_backend_test.go:110.
func newAPIRunBackend(app *appPkg.App) APIRunBackend { return api.NewAPIRunBackend(app) }

// newAPIRunBackendWithPolicy builds the run backend with a permission mode;
// callers api_run_backend_policy_test.go:24,99.
func newAPIRunBackendWithPolicy(app *appPkg.App, broker *APIPermissionBroker, allowAutoApprove bool) APIRunBackend {
	return api.NewAPIRunBackendWithPolicy(app, broker, allowAutoApprove)
}

// apiRunBackendSelection admits a pinned role/model selection; caller
// task76_api_run_backend_test.go:250.
func apiRunBackendSelection(cfg *config.Config, role, model string, selection *apiRunSelection) (config.SelectedModelType, *agent.ModelOverride, *session.RoleContract, error) {
	return api.APIRunBackendSelection(cfg, role, model, selection)
}

// validateAPIRunSelection validates a role/model pair; callers
// api_run_backend_test.go:28, task76_api_run_selection_admission_test.go:125.
func validateAPIRunSelection(cfg *config.Config, role, model string) (config.SelectedModelType, error) {
	return api.ValidateAPIRunSelection(cfg, role, model)
}

// apiRunCreateHandlerWithSnapshot builds the create-run handler over a config
// snapshot; callers api_run_custom_role_admission_test.go:73,98.
func apiRunCreateHandlerWithSnapshot(store APIWriteStore, backend APIRunBackend, registry *APIRunRegistry, snapshotFn func() (*config.Config, error)) http.HandlerFunc {
	return api.APIRunCreateHandlerWithSnapshot(store, backend, registry, snapshotFn)
}

// validAPIRunTransition reports whether a run state change is legal; caller
// api_runs_test.go:59.
func validAPIRunTransition(from, to APIRunState) bool { return api.ValidAPIRunTransition(from, to) }

// Events (owner: rest/api/events.go).

// NewAPIEventLog builds an event log; caller api_events_behavior_test.go:48.
func NewAPIEventLog(now func() time.Time) *APIEventLog { return api.NewAPIEventLog(now) }

// startAPIEventFeeder starts the App event feeder; callers api_serve.go:115,
// finalization_wiring_test.go:229.
func startAPIEventFeeder(app *appPkg.App, log *APIEventLog, registry *APIRunRegistry, gates ...session.FinalizationTerminalGate) *apiEventFeeder {
	return api.StartAPIEventFeeder(app, log, registry, gates...)
}

// newAPIEventsHandler builds the SSE handler; callers
// api_events_handler_test.go:62, finalization_wiring_test.go:364.
func newAPIEventsHandler(store api.APIEventsSessionStore, log *APIEventLog, limiter *apiSSELimiter, ticker api.APISSETickerFactory, gates ...session.FinalizationTerminalGate) http.HandlerFunc {
	return api.NewAPIEventsHandler(store, log, limiter, ticker, gates...)
}

// apiSSERevocation closes when the principal loses its token; caller
// api_events_revoke_test.go:31.
func apiSSERevocation(ctx context.Context, principal Principal) <-chan struct{} {
	return api.APISSERevocation(ctx, principal)
}

// Permissions (owner: rest/api/permissions.go).

// NewAPIPermissionBroker builds a broker; callers api_serve.go:75,
// api_permissions_test.go:78, api_permissions_handlers_test.go:32.
func NewAPIPermissionBroker(service permission.Service, workspace string, emit func(apiPermissionEvent), timeout ...time.Duration) *APIPermissionBroker {
	return api.NewAPIPermissionBroker(service, workspace, emit, timeout...)
}

// redactPermissionPath strips the workspace prefix; caller
// api_permissions_test.go:305.
func redactPermissionPath(path, workspace string) string {
	return api.RedactPermissionPath(path, workspace)
}

// Idempotency (owner: rest/api/permissions.go).

// newIdempotencyStore builds an idempotency store; callers
// api_idempotency_test.go:17, api_idempotency_expiry_test.go:14.
func newIdempotencyStore(now func() time.Time) *idempotencyStore {
	return api.NewIdempotencyStore(now)
}

// withIdempotency wraps a handler with idempotent replay; caller
// api_idempotency_test.go:135.
func withIdempotency(store *idempotencyStore, next http.Handler) http.Handler {
	return api.WithIdempotency(store, next)
}

// validIdempotencyKey reports whether a key is a well-formed idempotency key;
// caller api_idempotency_test.go:119.
func validIdempotencyKey(key string) bool { return api.ValidIdempotencyKey(key) }

// writeIdempotencyHashPart feeds a body part into the request hash; caller
// api_idempotency_test.go:271.
func writeIdempotencyHashPart(buffer *bytes.Buffer, part []byte) {
	api.WriteIdempotencyHashPart(buffer, part)
}

// Batch (owner: rest/apibatch/service.go).

// releaseAPIBatchService drops one reference to the App's batch service;
// caller api_serve.go:124.
func releaseAPIBatchService(app *appPkg.App, ctx context.Context) error {
	return apibatch.ReleaseAPIBatchService(app, ctx)
}

// serviceSessionID is the hidden service-session ID of one token; callers
// api_batch_intake_test.go:34, api_batch_fixture_diag_test.go:112.
func serviceSessionID(tokenID string) string { return apibatch.ServiceSessionID(tokenID) }

// apiauth_bridge.go: temporary compatibility surface for package server after
// the S2 seam moved the api-authentication leaf to internal/serve/server/apiauth
// (sprint 0 lane L2). Design lanes L6 (rest) and L7 (composer) are the
// deleters: as their consumers move out of package server, the aliases below
// disappear with them. Type aliases and const re-exports only — no mutable
// var re-exports (the two deleted S2 var fallbacks must not reappear in any
// form).

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

// listener_bridge.go: temporary compatibility surface for package server after
// lane L3 moved the listener policy + TLS leaf to
// internal/serve/server/listener (sprint 0 split, design
// docs/plans/2026-11-15-sprint0-split-server.md §2.2/§2.3). Deleter lane: L7
// removes the last names (composer + cmd aliases §2.3); consumers migrate in
// lanes L4-L6. Type aliases and const re-exports only for types/constants;
// one-line delegating wrappers for functions — no mutable var re-exports.

type (
	// Core L3 surface (design §2.2 / §2.3 cmd aliases).
	ListenerPolicy      = listener.ListenerPolicy
	ListenerSurface     = listener.ListenerSurface
	ListenerTLS         = listener.ListenerTLS
	ListenerLimits      = listener.ListenerLimits
	ListenerPolicyError = listener.ListenerPolicyError
	TLSPins             = listener.TLSPins
	// tlsCertificateStore aliases the designed port type (listener/port.go);
	// the Server.tlsStore field (server.go:40) keeps its name until L7.
	tlsCertificateStore = listener.TLSCertificateStore
)

const (
	SurfaceREST = listener.SurfaceREST
	SurfaceWS   = listener.SurfaceWS
	SurfaceUI   = listener.SurfaceUI
)

func DenyAllAuth(next http.Handler) http.Handler { return listener.DenyAllAuth(next) }

func ValidateListenerSet(policies []ListenerPolicy) error {
	return listener.ValidateListenerSet(policies)
}

func EffectiveListenerPins(declared map[string]string, tokens []config.APIToken, logger *slog.Logger) (map[string]string, error) {
	return listener.EffectiveListenerPins(declared, tokens, logger)
}

func ValidatePinnedPermissionModes(tokens []config.APIToken, pins map[string]string) error {
	return listener.ValidatePinnedPermissionModes(tokens, pins)
}

func ValidatePinnedPermissionModesAcrossPolicies(tokens []config.APIToken, policies []ListenerPolicy) error {
	return listener.ValidatePinnedPermissionModesAcrossPolicies(tokens, policies)
}

func PinnedTokenIDs(pins map[string]string) []string { return listener.PinnedTokenIDs(pins) }

func ListenerDeclaresPins(pins map[string]string) bool { return listener.ListenerDeclaresPins(pins) }

func ValidateEffectivePins(ctx context.Context, app *appPkg.App, tokens []config.APIToken, policies []ListenerPolicy, logger *slog.Logger) error {
	return listener.ValidateEffectivePins(ctx, app, tokens, policies, logger)
}

func EnsureSelfSignedCertificate(dataDir, address string, sans []string, now time.Time) (string, string, error) {
	return listener.EnsureSelfSignedCertificate(dataDir, address, sans, now)
}

func EnsureSelfSignedCertificateForListener(dataDir, identityAddress, address string, sans []string, now time.Time) (string, string, error) {
	return listener.EnsureSelfSignedCertificateForListener(dataDir, identityAddress, address, sans, now)
}

func TLSPinsForCertificatePEM(data []byte) (TLSPins, error) {
	return listener.TLSPinsForCertificatePEM(data)
}

func IsLoopbackHost(host string) bool { return listener.IsLoopbackHost(host) }

func OriginsAllowed(origin string, allowed []string) bool {
	return listener.OriginsAllowed(origin, allowed)
}

func ValidateAllowedOrigins(origins []string) error { return listener.ValidateAllowedOrigins(origins) }

// TLS machinery wrappers (designed port in listener/port.go): the composer
// (server.go) and the TLS white tests that stay in package server until L5/L7
// keep their unqualified call sites.

func newTLSCertificateStore(policy ListenerTLS) (*tlsCertificateStore, error) {
	return listener.NewTLSCertificateStore(policy)
}

func tlsConfig(policy ListenerTLS, store *tlsCertificateStore) (*tls.Config, error) {
	return listener.TLSConfig(policy, store)
}

func runTLSReload(ctx context.Context, store *tlsCertificateStore) { listener.TLSReload(ctx, store) }

func tlsErrorLogger() *log.Logger { return listener.TLSErrorLogger() }

func loadClientCAPool(path string) (*x509.CertPool, error) { return listener.LoadClientCAPool(path) }
