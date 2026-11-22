package api

// compat.go: temporary compatibility surface for package api after the rest
// handlers moved into internal/serve/server/rest/api (sprint 0 split, design
// docs/plans/2026-11-15-sprint0-split-server.md). Same technique as the root
// bridges: type aliases, const re-exports and one-line delegating wrappers,
// each copied verbatim from the root bridge file that declares it today.

import (
	"context"
	"net/http"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
	"github.com/PHPCraftdream/rush/internal/serve/server/hub"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsapi"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsconfig"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// Const re-exports of the wire run-state values (wire_bridge.go).
const (
	APIRunQueued         = wire.APIRunQueued
	APIRunRunning        = wire.APIRunRunning
	APIRunSucceeded      = wire.APIRunSucceeded
	APIRunFailed         = wire.APIRunFailed
	APIRunCancelled      = wire.APIRunCancelled
	APIRunAwaitingAnswer = wire.APIRunAwaitingAnswer
)

// Type aliases of the wire protocol types (wire_bridge.go).
type (
	APIEvent    = wire.APIEvent
	APIMessage  = wire.APIMessage
	APIModel    = wire.APIModel
	APIRunState = wire.APIRunState
	APISession  = wire.APISession
	ConfigWire  = wire.ConfigWire
)

// Type aliases of the api-authentication leaf types (apiauth_bridge.go).
type (
	APIProblem = apiauth.APIProblem
	Pagination = apiauth.Pagination
	Principal  = apiauth.Principal
	Ticker     = apiauth.Ticker
)

// Function re-exports of the api-authentication leaf (apiauth_bridge.go).

func ParsePagination(r *http.Request) (Pagination, error) { return apiauth.ParsePagination(r) }

func PrincipalFrom(ctx context.Context) (Principal, bool) { return apiauth.PrincipalFrom(ctx) }

func RealTicker(d time.Duration) Ticker { return apiauth.RealTicker(d) }

func Redact(s string) string { return apiauth.Redact(s) }

func ResolveBatchOnlyRoute(operationID string) int { return apiauth.ResolveBatchOnlyRoute(operationID) }

func ResolvePinnedRoute(pin, operationID, pathID string) int {
	return apiauth.ResolvePinnedRoute(pin, operationID, pathID)
}

func StillValid(p Principal, now time.Time) bool { return apiauth.StillValid(p, now) }

func WriteAPIProblem(w http.ResponseWriter, r *http.Request, status int, title, detail, instance string, cause error) {
	apiauth.WriteAPIProblem(w, r, status, title, detail, instance, cause)
}

func WriteAPIResponse(w http.ResponseWriter, r *http.Request, status int, payload any) {
	apiauth.WriteAPIResponse(w, r, status, payload)
}

func WritePaginatedResponse(w http.ResponseWriter, r *http.Request, status int, items any, ids []string, hasMore bool) {
	apiauth.WritePaginatedResponse(w, r, status, items, ids, hasMore)
}

// SetAPIResponseHeaders delegates to the apiauth export added for this
// package (apiauth/response.go; renamed from apiauth_bridge.go's unexported
// copy).
func SetAPIResponseHeaders(w http.ResponseWriter, r *http.Request) {
	apiauth.SetAPIResponseHeaders(w, r)
}

// Function re-exports of the owning sibling packages.

// buildConfigWire delegates to wsconfig (ws_bridge.go).
func buildConfigWire(a *appPkg.App) (ConfigWire, bool) {
	return wsconfig.BuildConfigWire(a)
}

// holdExternalSilenceProof delegates to wsapi (ws_bridge.go).
func holdExternalSilenceProof(a *appPkg.App, sessionID string) (*sessionlock.SharedLockProbe, bool, string) {
	return wsapi.HoldExternalSilenceProof(a, sessionID)
}

// pinnedSessionsOnly delegates to hub (hub_bridge.go).
func pinnedSessionsOnly(sessions []session.Session, pin string) []session.Session {
	return hub.PinnedSessionsOnly(sessions, pin)
}
