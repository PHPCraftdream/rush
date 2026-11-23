package apibatch

// compat.go: temporary compatibility surface for package apibatch after the
// batch handlers moved out of package server into
// internal/serve/server/rest/apibatch (sprint 0 split, design
// docs/plans/2026-11-15-sprint0-split-server.md). Same technique as the root
// bridges: type aliases, const re-exports and one-line delegating wrappers,
// each copied verbatim from the file that owns the declaration today.

import (
	"context"
	"net/http"

	"github.com/PHPCraftdream/rush/internal/serve/server/apiauth"
	"github.com/PHPCraftdream/rush/internal/serve/server/rest/api"
)

// Type aliases of the rest/api route declarations (owner: rest/api/routes.go).
type APIRoute = api.APIRoute

// Type alias of the api-authentication leaf type (owner: apiauth_bridge.go).
type Principal = apiauth.Principal

// Function re-exports of the api-authentication leaf (owner:
// apiauth_bridge.go; signatures copied verbatim from that file).
func PrincipalFrom(ctx context.Context) (Principal, bool) { return apiauth.PrincipalFrom(ctx) }

func WriteAPIProblem(w http.ResponseWriter, r *http.Request, status int, title, detail, instance string, cause error) {
	apiauth.WriteAPIProblem(w, r, status, title, detail, instance, cause)
}

func WriteAPIResponse(w http.ResponseWriter, r *http.Request, status int, payload any) {
	apiauth.WriteAPIResponse(w, r, status, payload)
}

// Const re-exports of the run body limits (owner: rest/api/runhandlers.go).
const (
	apiRunMaxBody       = api.ApiRunMaxBody
	apiRunMaxFieldBytes = api.ApiRunMaxFieldBytes
)

// Const re-export of the API base path (owner: rest/api/routes.go).
const apiBasePath = api.ApiBasePath

// Function re-exports of the rest/api helpers the moved bodies still call
// unqualified (one-line delegations, no bodies of their own).

// apiDecodeStrictBody (owner: rest/api/runhandlers.go).
func apiDecodeStrictBody(r *http.Request, target any) error {
	return api.ApiDecodeStrictBody(r, target)
}

// apiNotFound (owner: rest/api/read.go).
func apiNotFound(w http.ResponseWriter, r *http.Request, kind string) {
	api.ApiNotFound(w, r, kind)
}

// apiReadFailure (owner: rest/api/read.go).
func apiReadFailure(w http.ResponseWriter, r *http.Request, err error) {
	api.ApiReadFailure(w, r, err)
}

// validIdempotencyKey (owner: rest/api/permissions.go).
func validIdempotencyKey(key string) bool { return api.ValidIdempotencyKey(key) }
