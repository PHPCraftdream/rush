package server

import "testing"

func TestAPIRoutesExhaustive(t *testing.T) {
	routes := APIRoutes()
	if len(routes) != 3 {
		t.Fatalf("routes=%d", len(routes))
	}
	seen, operations := map[string]bool{}, map[string]bool{}
	for _, route := range routes {
		if route.Method == "" || route.Pattern == "" || route.Pattern[0] != '/' || route.Handler == nil {
			t.Fatalf("invalid route: %+v", route)
		}
		if route.Scope != "none" && route.Scope != "read" && route.Scope != "run" && route.Scope != "config" && route.Scope != "admin" {
			t.Fatalf("invalid scope %q", route.Scope)
		}
		key := route.Method + " " + route.Pattern
		if seen[key] {
			t.Fatalf("duplicate route %s", key)
		}
		seen[key] = true
		if route.OperationID == "" || operations[route.OperationID] {
			t.Fatalf("invalid operation id %q", route.OperationID)
		}
		operations[route.OperationID] = true
	}
	copy := APIRoutes()
	copy[0].Pattern = "/mutated"
	if APIRoutes()[0].Pattern == "/mutated" {
		t.Fatal("route table was mutable")
	}
}
