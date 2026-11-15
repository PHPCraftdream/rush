package server

import "net/http"

func apiSessionRoutes() []APIRoute {
	return []APIRoute{
		{Method: http.MethodPost, Pattern: "/sessions", Scope: "run", Summary: "Create session", OperationID: "createSession", Handler: apiSessionPlaceholder, Params: []APIParam{{Name: "Idempotency-Key", In: "header", Description: "Optional request idempotency key"}}, ResponseDescription: "Created session"},
		{Method: http.MethodPost, Pattern: "/sessions/{id}/fork", Scope: "run", Summary: "Fork session", OperationID: "forkSession", Handler: apiSessionPlaceholder, Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Session ID"}, {Name: "Idempotency-Key", In: "header", Description: "Optional request idempotency key"}}, ResponseDescription: "Forked session"},
		{Method: http.MethodDelete, Pattern: "/sessions/{id}", Scope: "run", Summary: "Delete session", OperationID: "deleteSession", Handler: apiSessionPlaceholder, Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Session ID"}}, ResponseDescription: "Session deleted"},
	}
}

func apiSessionPlaceholder(w http.ResponseWriter, r *http.Request) {
	WriteAPIProblem(w, r, http.StatusNotImplemented, "Not Implemented", "session endpoint handler is not wired", "", nil)
}
