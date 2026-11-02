package server

import "net/http"

func apiRunRoutes() []APIRoute {
	return []APIRoute{
		{Method: http.MethodPost, Pattern: "/sessions/{id}/runs", Scope: "run", Summary: "Create run", OperationID: "createRun", Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Session ID"}, {Name: "Idempotency-Key", In: "header", Description: "Optional request idempotency key"}}, ResponseDescription: "Run accepted"},
		{Method: http.MethodGet, Pattern: "/runs/{id}", Scope: "read", Summary: "Get run", OperationID: "getRun", Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Run ID"}, {Name: "wait", In: "query", Description: "Wait up to 30 seconds for a state change"}}, ResponseDescription: "Run status"},
		{Method: http.MethodPost, Pattern: "/runs/{id}/cancel", Scope: "run", Summary: "Cancel run", OperationID: "cancelRun", Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Run ID"}}, ResponseDescription: "Run cancellation status"},
		{Method: http.MethodPost, Pattern: "/sessions/{id}/inject", Scope: "run", Summary: "Inject message", OperationID: "injectMessage", Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Session ID"}}, ResponseDescription: "Message accepted"},
	}
}

func apiRunWriteRoutes(store APIWriteStore, backend APIRunBackend) []APIRoute {
	return apiRunWriteRoutesWithRegistry(store, backend, NewAPIRunRegistry(nil))
}

func apiRunWriteRoutesWithRegistry(store APIWriteStore, backend APIRunBackend, registry *APIRunRegistry) []APIRoute {
	idempotency := newIdempotencyStore(nil)
	routes := apiRunRoutes()
	routes[0].Handler = withIdempotency(idempotency, http.HandlerFunc(apiCreateRunHandler(store, backend, registry))).ServeHTTP
	routes[1].Handler = apiGetRunHandler(registry)
	routes[2].Handler = apiCancelRunHandler(registry, backend)
	routes[3].Handler = apiInjectHandler(store, backend, registry)
	return routes
}

func apiRunPlaceholder(w http.ResponseWriter, r *http.Request) {
	WriteAPIProblem(w, r, http.StatusNotImplemented, "Not Implemented", "run endpoint handler is not wired", "", nil)
}

func apiRunRoutesWithPlaceholders() []APIRoute {
	routes := apiRunRoutes()
	for i := range routes {
		routes[i].Handler = apiRunPlaceholder
	}
	return routes
}
