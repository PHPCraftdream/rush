package server

import (
	"errors"
	"net/http"

	"github.com/PHPCraftdream/rush/internal/config"
)

type apiPermissionAnswer struct {
	Decision string `json:"decision"`
}

func apiPermissionRoutes(broker *APIPermissionBroker) []APIRoute {
	routes := apiPermissionRouteDeclarations()
	routes[0].Handler = apiListPermissionsHandler(broker)
	routes[1].Handler = apiAnswerPermissionHandler(broker)
	return routes
}

func apiListPermissionsHandler(broker *APIPermissionBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFrom(r.Context())
		if !ok || broker == nil {
			apiNotFound(w, r, "permission")
			return
		}
		sessions, present := r.URL.Query()["session"]
		if present && len(sessions) != 1 {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "session must be specified once", "", nil)
			return
		}
		admin := config.APITokenScopeAllows(principal.Scopes, "admin")
		items := broker.Pending(principal.TokenID, admin, r.URL.Query().Get("session"))
		WriteAPIResponse(w, r, http.StatusOK, items)
	}
}

func apiAnswerPermissionHandler(broker *APIPermissionBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload apiPermissionAnswer
		if err := apiDecodeStrictBody(r, &payload); err != nil || (payload.Decision != "allow" && payload.Decision != "deny") {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid permission decision", "", nil)
			return
		}
		principal, ok := PrincipalFrom(r.Context())
		if !ok || broker == nil {
			apiNotFound(w, r, "permission")
			return
		}
		err := broker.Answer(r.PathValue("id"), principal.TokenID, payload.Decision, config.APITokenScopeAllows(principal.Scopes, "admin"))
		switch {
		case errors.Is(err, ErrAPIPermissionUnknown):
			apiNotFound(w, r, "permission")
		case errors.Is(err, ErrAPIPermissionDecided):
			WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "permission already decided", "", nil)
		case err != nil:
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid permission decision", "", nil)
		default:
			WriteAPIResponse(w, r, http.StatusOK, struct {
				Decision string `json:"decision"`
			}{payload.Decision})
		}
	}
}
