package server

import "testing"

func TestAPIRoutesExhaustive(t *testing.T) {
	routes := APIRoutes()
	if len(routes) != 14 {
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
	want := map[string]string{
		"POST /sessions/{id}/runs":   "createRun",
		"GET /runs/{id}":             "getRun",
		"POST /runs/{id}/cancel":     "cancelRun",
		"POST /sessions/{id}/inject": "injectMessage",
	}
	for _, route := range routes {
		key := route.Method + " " + route.Pattern
		if operation, ok := want[key]; ok {
			if route.OperationID != operation {
				t.Errorf("%s operationId=%q want %q", key, route.OperationID, operation)
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Errorf("missing pinned run routes: %v", want)
	}
}
