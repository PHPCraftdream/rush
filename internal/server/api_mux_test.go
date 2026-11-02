package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIMuxErrorsAndGuard(t *testing.T) {
	invoked := 0
	routes := []APIRoute{
		{Method: "GET", Pattern: "/x", Scope: "read", Handler: func(w http.ResponseWriter, r *http.Request) { invoked++; w.WriteHeader(204) }},
		{Method: "POST", Pattern: "/x", Scope: "read", Handler: func(w http.ResponseWriter, r *http.Request) { invoked++; w.WriteHeader(204) }},
	}
	handler := NewAPIHandler(routes)
	for _, tc := range []struct {
		method, path string
		status       int
		allow        string
	}{
		{"GET", "/api/v1/missing", 404, ""}, {"PUT", "/api/v1/x", 405, "GET, POST"},
	} {
		req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, nil)
		req.Header.Set("X-Request-Id", "safe_id-1")
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != tc.status || res.Header().Get("X-Request-Id") != "safe_id-1" || res.Header().Get("Allow") != tc.allow {
			t.Fatalf("%s %s: %d %q", tc.method, tc.path, res.Code, res.Header().Get("Allow"))
		}
	}
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/x", nil)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != 500 || invoked != 0 {
		t.Fatalf("missing principal: status=%d invoked=%d", res.Code, invoked)
	}
}

func TestAPIMuxSplitHidesProtectedRoutes(t *testing.T) {
	routes := APIRoutes()
	public, protected := NewAPIHandlerSplit(routes)
	for _, tc := range []struct {
		h      http.Handler
		path   string
		status int
	}{
		{public, "/api/v1/health", 200}, {public, "/api/v1/version", 404}, {protected, "/api/v1/health", 404},
	} {
		res := httptest.NewRecorder()
		tc.h.ServeHTTP(res, httptest.NewRequestWithContext(context.Background(), "GET", tc.path, nil))
		if res.Code != tc.status {
			t.Fatalf("%s: %d", tc.path, res.Code)
		}
	}
}
