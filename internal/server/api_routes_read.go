package server

import "net/http"

// apiReadRoutes declares the read-only REST surface.
func apiReadRoutes() []APIRoute {
	return []APIRoute{
		{
			Method: http.MethodGet, Pattern: "/sessions", Scope: "read",
			Summary: "List sessions", OperationID: "listSessions",
			Handler: apiReadPlaceholder,
			Params: []APIParam{
				{Name: "after", In: "query", Description: "Cursor session ID"},
				{Name: "limit", In: "query", Description: "Maximum number of results (1-200; default 50)"},
			},
			ResponseDescription: "A page of safe session summaries",
		},
		{
			Method: http.MethodGet, Pattern: "/sessions/{id}", Scope: "read",
			Summary: "Get session", OperationID: "getSession",
			Handler:             apiReadPlaceholder,
			Params:              []APIParam{{Name: "id", In: "path", Required: true, Description: "Session ID"}},
			ResponseDescription: "A safe session summary",
		},
		{
			Method: http.MethodGet, Pattern: "/sessions/{id}/messages", Scope: "read",
			Summary: "List session messages", OperationID: "listMessages",
			Handler: apiReadPlaceholder,
			Params: []APIParam{
				{Name: "id", In: "path", Required: true, Description: "Session ID"},
				{Name: "after", In: "query", Description: "Cursor message ID"},
				{Name: "limit", In: "query", Description: "Maximum number of results (1-200; default 50)"},
			},
			ResponseDescription: "A page of safe message projections",
		},
		{
			Method: http.MethodGet, Pattern: "/models", Scope: "read",
			Summary: "List models", OperationID: "listModels",
			Handler:             apiReadPlaceholder,
			ResponseDescription: "Available model identifiers and names",
		},
	}
}

func apiReadPlaceholder(w http.ResponseWriter, r *http.Request) {
	WriteAPIProblem(w, r, http.StatusNotImplemented, "Not Implemented", "read endpoint handler is not wired", "", nil)
}
