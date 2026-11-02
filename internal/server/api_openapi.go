package server

import (
	"net/http"
	"sort"

	"github.com/PHPCraftdream/rush/internal/version"
)

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
			"responses":    openAPIResponses(route.Method, route.ResponseDescription),
			"x-rush-scope": route.Scope,
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
		pathItem[lowercaseASCII(route.Method)] = operation
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

func openAPIResponses(method, successDescription string) map[string]any {
	if successDescription == "" {
		successDescription = "Success"
	}
	var successStatus string
	var successResponse map[string]any
	switch method {
	case http.MethodPost:
		successStatus = "201"
		successResponse = map[string]any{"description": successDescription, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
	case http.MethodDelete:
		successStatus = "204"
		successResponse = map[string]any{"description": successDescription}
	default:
		successStatus = "200"
		successResponse = map[string]any{"description": successDescription, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}}}
	}
	responses := map[string]any{successStatus: successResponse}
	for _, code := range []string{"400", "401", "403", "404", "405", "409", "413", "422", "429", "500"} {
		responses[code] = map[string]any{"description": httpStatusDescription(code), "content": map[string]any{"application/problem+json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Problem"}}}}
	}
	return responses
}

func lowercaseASCII(value string) string {
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
