package server

import "net/http"

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
	return routes
}

func apiCoreRoutes() []APIRoute {
	return []APIRoute{
		{Method: http.MethodGet, Pattern: "/health", Scope: "none", Summary: "Health check", OperationID: "getHealth", Handler: apiHealthHandler},
		{Method: http.MethodGet, Pattern: "/version", Scope: "read", Summary: "Get version", OperationID: "getVersion", Handler: apiVersionHandler},
		{Method: http.MethodGet, Pattern: "/openapi.json", Scope: "read", Summary: "OpenAPI document", OperationID: "getOpenAPI", Handler: apiOpenAPIHandler},
	}
}
