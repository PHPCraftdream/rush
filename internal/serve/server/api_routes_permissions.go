package server

import "net/http"

func apiPermissionRouteDeclarations() []APIRoute {
	return []APIRoute{
		{Method: http.MethodGet, Pattern: "/permissions", Scope: "run", Summary: "List pending permissions", OperationID: "listPermissions", Handler: apiPermissionPlaceholder, Params: []APIParam{{Name: "session", In: "query", Description: "Filter by session ID"}}, ResponseDescription: "Pending permission requests"},
		{Method: http.MethodPost, Pattern: "/permissions/{id}", Scope: "run", Summary: "Answer permission request", OperationID: "answerPermission", Handler: apiPermissionPlaceholder, Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Permission request ID"}}, ResponseDescription: "Permission decision recorded"},
	}
}

func apiPermissionPlaceholder(w http.ResponseWriter, r *http.Request) {
	WriteAPIProblem(w, r, http.StatusServiceUnavailable, "Service Unavailable", "permission service unavailable", "", nil)
}
