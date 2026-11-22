package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/diag/version"
)

type APIParam struct {
	Name        string
	In          string
	Required    bool
	Description string
}

type APIRoute struct {
	Method              string
	Pattern             string
	Scope               string
	Summary             string
	OperationID         string
	Handler             http.HandlerFunc
	Params              []APIParam
	ResponseDescription string
}

func APIRoutes() []APIRoute {
	routes := append(apiCoreRoutes(), apiReadRoutes()...)
	routes = append(routes, apiSessionRoutes()...)
	routes = append(routes, apiRunRoutesWithPlaceholders()...)
	routes = append(routes, apiPermissionRouteDeclarations()...)
	routes = append(routes, BatchRoutePlaceholders()...)
	return append(routes, apiEventsRoute())
}

func apiCoreRoutes() []APIRoute {
	return []APIRoute{
		{Method: http.MethodGet, Pattern: "/health", Scope: "none", Summary: "Health check", OperationID: "getHealth", Handler: apiHealthHandler},
		{Method: http.MethodGet, Pattern: "/version", Scope: "read", Summary: "Get version", OperationID: "getVersion", Handler: apiVersionHandler},
		{Method: http.MethodGet, Pattern: "/openapi.json", Scope: "read", Summary: "OpenAPI document", OperationID: "getOpenAPI", Handler: apiOpenAPIHandler},
	}
}

func apiHealthHandler(w http.ResponseWriter, r *http.Request) {
	WriteAPIResponse(w, r, http.StatusOK, struct {
		Status string `json:"status"`
	}{Status: "ok"})
}

func apiVersionHandler(w http.ResponseWriter, r *http.Request) {
	WriteAPIResponse(w, r, http.StatusOK, struct {
		Version string `json:"version"`
	}{Version: version.VersionLine()})
}

func apiOpenAPIHandler(w http.ResponseWriter, r *http.Request) {
	WriteAPIResponse(w, r, http.StatusOK, BuildOpenAPIDocument(APIRoutes()))
}

func apiPermissionRouteDeclarations() []APIRoute {
	return []APIRoute{
		{Method: http.MethodGet, Pattern: "/permissions", Scope: "run", Summary: "List pending permissions", OperationID: "listPermissions", Handler: apiPermissionPlaceholder, Params: []APIParam{{Name: "session", In: "query", Description: "Filter by session ID"}}, ResponseDescription: "Pending permission requests"},
		{Method: http.MethodPost, Pattern: "/permissions/{id}", Scope: "run", Summary: "Answer permission request", OperationID: "answerPermission", Handler: apiPermissionPlaceholder, Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Permission request ID"}}, ResponseDescription: "Permission decision recorded"},
	}
}

func apiPermissionPlaceholder(w http.ResponseWriter, r *http.Request) {
	WriteAPIProblem(w, r, http.StatusServiceUnavailable, "Service Unavailable", "permission service unavailable", "", nil)
}

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

func apiRunRoutes() []APIRoute {
	return []APIRoute{
		{Method: http.MethodPost, Pattern: "/sessions/{id}/runs", Scope: "run", Summary: "Create run", OperationID: "createRun", Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Session ID"}, {Name: "Idempotency-Key", In: "header", Description: "Optional request idempotency key"}}, ResponseDescription: "Run accepted"},
		{Method: http.MethodGet, Pattern: "/runs/{id}", Scope: "read", Summary: "Get run", OperationID: "getRun", Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Run ID"}, {Name: "wait", In: "query", Description: "Wait up to 30 seconds for a state change"}}, ResponseDescription: "Run status"},
		{Method: http.MethodPost, Pattern: "/runs/{id}/cancel", Scope: "run", Summary: "Cancel run", OperationID: "cancelRun", Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Run ID"}}, ResponseDescription: "Run cancellation status"},
		{Method: http.MethodPost, Pattern: "/sessions/{id}/inject", Scope: "run", Summary: "Inject message", OperationID: "injectMessage", Params: []APIParam{{Name: "id", In: "path", Required: true, Description: "Session ID"}}, ResponseDescription: "Message accepted"},
	}
}

func APIRunWriteRoutes(store APIWriteStore, backend APIRunBackend) []APIRoute {
	return apiRunWriteRoutesWithRegistry(store, backend, NewAPIRunRegistry(nil))
}

func apiRunWriteRoutesWithRegistry(store APIWriteStore, backend APIRunBackend, registry *APIRunRegistry) []APIRoute {
	idempotency := NewIdempotencyStore(nil)
	routes := apiRunRoutes()
	routes[0].Handler = WithIdempotency(idempotency, http.HandlerFunc(apiCreateRunHandler(store, backend, registry))).ServeHTTP
	routes[1].Handler = apiGetRunHandler(registry)
	routes[2].Handler = apiCancelRunHandler(registry, backend)
	routes[3].Handler = apiInjectHandler(store, backend, registry)
	return routes
}

func ApiRunPlaceholder(w http.ResponseWriter, r *http.Request) {
	WriteAPIProblem(w, r, http.StatusNotImplemented, "Not Implemented", "run endpoint handler is not wired", "", nil)
}

func apiRunRoutesWithPlaceholders() []APIRoute {
	routes := apiRunRoutes()
	for i := range routes {
		routes[i].Handler = ApiRunPlaceholder
	}
	return routes
}

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

const ApiBasePath = "/api/v1"

func NewAPIHandler(routes []APIRoute) http.Handler {
	return apiRouteMux(routes)
}

func NewAPIHandlerSplit(routes []APIRoute) (public http.Handler, protected http.Handler) {
	publicRoutes, protectedRoutes := make([]APIRoute, 0), make([]APIRoute, 0)
	for _, route := range routes {
		if route.Scope == "none" {
			publicRoutes = append(publicRoutes, route)
		} else {
			protectedRoutes = append(protectedRoutes, route)
		}
	}
	return apiRouteMux(publicRoutes), apiRouteMux(protectedRoutes)
}

func apiRouteMux(routes []APIRoute) http.Handler {
	mux := http.NewServeMux()
	methods := http.NewServeMux()
	methodSets := make(map[string]map[string]struct{})
	for _, route := range routes {
		path := ApiBasePath + route.Pattern
		wrapped := route
		mux.Handle(route.Method+" "+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protectAPIRoute(wrapped).ServeHTTP(w, r)
		}))
		if methodSets[path] == nil {
			methods.Handle(path, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			methodSets[path] = make(map[string]struct{})
		}
		methodSets[path][route.Method] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idResponse := &APITrackingWriter{ResponseWriter: w}
		SetAPIResponseHeaders(idResponse, r)
		idRequest := r.Clone(r.Context())
		idRequest.Header = r.Header.Clone()
		idRequest.Header.Set("X-Request-Id", idResponse.Header().Get("X-Request-Id"))
		defer func() {
			if value := recover(); value != nil {
				slog.Error("API handler panic", "panic", Redact(fmt.Sprint(value)))
				if !idResponse.wrote {
					WriteAPIProblem(idResponse, idRequest, http.StatusInternalServerError, "Internal Server Error", "", "", fmt.Errorf("handler panic"))
				}
			}
		}()

		_, pattern := mux.Handler(idRequest)
		if pattern == "" {
			_, pathPattern := methods.Handler(idRequest)
			available := methodSets[pathPattern]
			if len(available) == 0 {
				WriteAPIProblem(idResponse, idRequest, http.StatusNotFound, "Not Found", "", "", nil)
				return
			}
			allowed := make([]string, 0, len(available))
			for method := range available {
				allowed = append(allowed, method)
			}
			sort.Strings(allowed)
			idResponse.Header().Set("Allow", strings.Join(allowed, ", "))
			WriteAPIProblem(idResponse, idRequest, http.StatusMethodNotAllowed, "Method Not Allowed", "", "", nil)
			return
		}
		// ServeMux.ServeHTTP, not the looked-up handler, fills r.PathValue.
		mux.ServeHTTP(idResponse, idRequest)
	})
}

// Unwrap lets http.ResponseController reach Flush (SSE) and Hijack.
func (w *APITrackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type APITrackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *APITrackingWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *APITrackingWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func protectAPIRoute(route APIRoute) http.Handler {
	if route.Scope == "none" {
		return route.Handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			WriteAPIProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "", "", fmt.Errorf("missing authenticated principal"))
			return
		}
		if status := ResolvePinnedRoute(p.PinnedSession, route.OperationID, r.PathValue("id")); status != 0 {
			if status == http.StatusNotFound {
				WriteAPIProblem(w, r, status, "Not Found", "resource not found", "", nil)
			} else {
				WriteAPIProblem(w, r, status, "Forbidden", "pinned-token", "", nil)
			}
			return
		}
		if p.BatchOnly && ResolveBatchOnlyRoute(route.OperationID) != 0 {
			WriteAPIProblem(w, r, http.StatusForbidden, "Forbidden", "batch-only", "", nil)
			return
		}
		if !config.APITokenScopeAllows(p.Scopes, route.Scope) {
			WriteAPIProblem(w, r, http.StatusForbidden, "Forbidden", "insufficient scope", "", nil)
			return
		}
		route.Handler.ServeHTTP(w, r)
	})
}

func BuildOpenAPIDocument(routes []APIRoute) map[string]any {
	paths := make(map[string]any)
	ordered := append([]APIRoute(nil), routes...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Pattern == ordered[j].Pattern {
			return ordered[i].Method < ordered[j].Method
		}
		return ordered[i].Pattern < ordered[j].Pattern
	})
	for _, route := range ordered {
		operation := map[string]any{
			"operationId":  route.OperationID,
			"summary":      route.Summary,
			"responses":    openAPIResponses(route.Method, route.OperationID, route.ResponseDescription),
			"x-rush-scope": route.Scope,
		}
		if route.OperationID == "streamSessionEvents" {
			operation["responses"] = openAPIStreamResponses(route.ResponseDescription)
		}
		if route.Scope == "none" {
			operation["security"] = []any{}
		} else {
			operation["security"] = []any{map[string]any{"bearerAuth": []any{}}}
		}
		if len(route.Params) > 0 {
			params := make([]any, 0, len(route.Params))
			for _, param := range route.Params {
				params = append(params, map[string]any{"name": param.Name, "in": param.In, "required": param.Required, "description": param.Description, "schema": map[string]any{"type": "string"}})
			}
			operation["parameters"] = params
		}
		pathItem, _ := paths[route.Pattern].(map[string]any)
		if pathItem == nil {
			pathItem = make(map[string]any)
			paths[route.Pattern] = pathItem
		}
		pathItem[LowercaseASCII(route.Method)] = operation
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info":    map[string]any{"title": "Rush API", "version": version.VersionLine()},
		"paths":   paths,
		"components": map[string]any{
			"securitySchemes": map[string]any{"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"}},
			"schemas":         map[string]any{"Problem": map[string]any{"type": "object", "properties": map[string]any{"type": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "status": map[string]any{"type": "integer"}, "detail": map[string]any{"type": "string"}, "instance": map[string]any{"type": "string"}}}},
		},
	}
}

func routeSuccessAccepted(operationID string) bool {
	return operationID == "createRun" || operationID == "cancelRun" || operationID == "injectMessage"
}

func openAPIStreamResponses(description string) map[string]any {
	if description == "" {
		description = "Session event stream"
	}
	responses := openAPIResponses(http.MethodGet, "streamSessionEvents", description)
	responses["200"] = map[string]any{"description": description, "content": map[string]any{"text/event-stream": map[string]any{"schema": map[string]any{"type": "string"}}}}
	return responses
}

func openAPIResponses(method, operationID, successDescription string) map[string]any {
	if successDescription == "" {
		successDescription = "Success"
	}
	var successStatus string
	var successResponse map[string]any
	switch method {
	case http.MethodPost:
		successStatus = "201"
		if routeSuccessAccepted(operationID) {
			successStatus = "202"
		} else if operationID == "answerPermission" {
			successStatus = "200"
		}
		successResponse = map[string]any{"description": successDescription, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
	case http.MethodDelete:
		successStatus = "204"
		successResponse = map[string]any{"description": successDescription}
	default:
		successStatus = "200"
		successResponse = map[string]any{"description": successDescription, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
	}
	responses := map[string]any{successStatus: successResponse}
	if operationID == "cancelRun" {
		responses["200"] = map[string]any{"description": successDescription, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
	}
	for _, code := range []string{"400", "401", "403", "404", "405", "409", "413", "422", "429", "500"} {
		responses[code] = map[string]any{"description": httpStatusDescription(code), "content": map[string]any{"application/problem+json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Problem"}}}}
	}
	return responses
}

func LowercaseASCII(value string) string {
	bytes := []byte(value)
	for i := range bytes {
		if bytes[i] >= 'A' && bytes[i] <= 'Z' {
			bytes[i] += 'a' - 'A'
		}
	}
	return string(bytes)
}

func httpStatusDescription(code string) string {
	switch code {
	case "400":
		return "Bad Request"
	case "401":
		return "Unauthorized"
	case "403":
		return "Forbidden"
	case "404":
		return "Not Found"
	case "405":
		return "Method Not Allowed"
	case "409":
		return "Conflict"
	case "413":
		return "Request Entity Too Large"
	case "422":
		return "Unprocessable Entity"
	case "429":
		return "Too Many Requests"
	default:
		return "Internal Server Error"
	}
}

func BatchRouteDeclarations() []APIRoute {
	root := APIParam{Name: "root", In: "path", Required: true, Description: "Batch root ID"}
	node := APIParam{Name: "node", In: "path", Required: true, Description: "Batch node ID"}
	key := APIParam{Name: "Idempotency-Key", In: "header", Description: "Optional request idempotency key; a replay returns the original root"}
	return []APIRoute{
		{Method: http.MethodPost, Pattern: "/batches", Scope: "run", Summary: "Submit batch", OperationID: "createBatch", Params: []APIParam{key}, ResponseDescription: "Batch accepted"},
		{Method: http.MethodPost, Pattern: "/tasks", Scope: "run", Summary: "Submit one agent task", OperationID: "createTask", Params: []APIParam{key}, ResponseDescription: "Task accepted as a one-item batch"},
		{Method: http.MethodGet, Pattern: "/batches/{root}", Scope: "read", Summary: "Get batch", OperationID: "getBatch", Params: []APIParam{root}, ResponseDescription: "Batch state and nodes"},
		{Method: http.MethodGet, Pattern: "/batches/{root}/nodes/{node}", Scope: "read", Summary: "Get batch node", OperationID: "getBatchNode", Params: []APIParam{root, node}, ResponseDescription: "Batch node"},
		{Method: http.MethodPost, Pattern: "/batches/{root}/stop", Scope: "run", Summary: "Stop batch", OperationID: "stopBatch", Params: []APIParam{root}, ResponseDescription: "Stop request queued"},
		{Method: http.MethodPost, Pattern: "/batches/{root}/nodes/{node}/stop", Scope: "run", Summary: "Stop batch node", OperationID: "stopBatchNode", Params: []APIParam{root, node}, ResponseDescription: "Stop request queued"},
		{Method: http.MethodPost, Pattern: "/batches/{root}/nodes/{node}/answer", Scope: "run", Summary: "Answer a held question", OperationID: "answerBatchQuestion", Params: []APIParam{root, node}, ResponseDescription: "Answer delivered to the existing node execution"},
	}
}

func BatchRoutePlaceholders() []APIRoute {
	routes := BatchRouteDeclarations()
	for i := range routes {
		routes[i].Handler = ApiRunPlaceholder
	}
	return routes
}
