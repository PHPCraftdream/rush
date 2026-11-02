package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIScopeGuardMatrix(t *testing.T) {
	scopes := []string{"read", "run", "config", "admin", "none"}
	principals := [][]string{{"read"}, {"run"}, {"config"}, {"admin"}, {}, {"bogus"}}
	want := [][]bool{{true, false, false, false, true}, {true, true, false, false, true}, {true, true, true, false, true}, {true, true, true, true, true}, {false, false, false, false, true}, {false, false, false, false, true}}
	for i, have := range principals {
		for j, need := range scopes {
			called := 0
			h := NewAPIHandler([]APIRoute{{Method: "GET", Pattern: "/scope", Scope: need, Handler: func(w http.ResponseWriter, _ *http.Request) { called++; w.WriteHeader(204) }}})
			r := httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/scope", nil).WithContext(WithPrincipal(httptest.NewRequestWithContext(context.Background(), "GET", "/", nil).Context(), Principal{Scopes: have}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if want[i][j] {
				if w.Code != 204 || called != 1 {
					t.Fatalf("have=%v need=%s status=%d called=%d", have, need, w.Code, called)
				}
			} else if w.Code != 403 || called != 0 || w.Body.String() != "{\"type\":\"about:blank\",\"title\":\"Forbidden\",\"status\":403,\"detail\":\"insufficient scope\"}\n" || w.Header().Get("Content-Type") != "application/problem+json" {
				t.Fatalf("have=%v need=%s status=%d body=%q called=%d", have, need, w.Code, w.Body.String(), called)
			}
		}
	}
	called := 0
	h := NewAPIHandler([]APIRoute{{Method: "GET", Pattern: "/scope", Scope: "read", Handler: func(http.ResponseWriter, *http.Request) { called++ }}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/scope", nil))
	if w.Code != 500 || called != 0 || w.Body.String() != "{\"type\":\"about:blank\",\"title\":\"Internal Server Error\",\"status\":500}\n" {
		t.Fatalf("missing principal: %d %q", w.Code, w.Body.String())
	}
}

func TestAPIMuxAllowHeaderSorted(t *testing.T) {
	routes := []APIRoute{}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		routes = append(routes, APIRoute{Method: method, Pattern: "/x", Scope: "none", Handler: func(http.ResponseWriter, *http.Request) {}})
	}
	h := NewAPIHandler(routes)
	var w *httptest.ResponseRecorder
	// Map iteration order is random per request: repeat so an unsorted
	// implementation cannot pass by luck.
	for range 100 {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), "PUT", "/api/v1/x", nil))
		if w.Code != 405 || w.Header().Get("Allow") != "DELETE, GET, POST" {
			t.Fatalf("status=%d Allow=%q", w.Code, w.Header().Get("Allow"))
		}
	}
	for _, path := range []string{"/api/v1/unknown", "/other"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), "GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestAPIHandlerNotFoundPassesThrough(t *testing.T) {
	body := "{\"own\":\"not found\"}\n"
	routes := []APIRoute{{Method: "GET", Pattern: "/x", Scope: "read", Handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(body))
	}}, {Method: "POST", Pattern: "/x", Scope: "read", Handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", "CUSTOM")
		w.WriteHeader(405)
		_, _ = w.Write([]byte("custom method"))
	}}}
	h := NewAPIHandler(routes)
	for _, tc := range []struct {
		method, body, allow string
		status              int
	}{{"GET", body, "", 404}, {"POST", "custom method", "CUSTOM", 405}} {
		r := httptest.NewRequestWithContext(context.Background(), tc.method, "/api/v1/x", nil)
		r = r.WithContext(WithPrincipal(r.Context(), Principal{Scopes: []string{"read"}}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status || w.Body.String() != tc.body || w.Header().Get("Allow") != tc.allow {
			t.Fatalf("%s: status=%d allow=%q body=%q", tc.method, w.Code, w.Header().Get("Allow"), w.Body.String())
		}
	}
}

func TestAPIPanicIsGenericAndRedacted(t *testing.T) {
	secret := "rush_12345678." + strings.Repeat("A", 43)
	capture := &captureHandler{notify: make(chan struct{}, 1)}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	defer slog.SetDefault(previous)
	h := NewAPIHandler([]APIRoute{{Method: "GET", Pattern: "/panic", Scope: "none", Handler: func(http.ResponseWriter, *http.Request) { panic("panic-marker " + secret) }}})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/panic", nil))
	if w.Code != 500 || w.Body.String() != "{\"type\":\"about:blank\",\"title\":\"Internal Server Error\",\"status\":500}\n" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Values("Content-Type"); len(got) != 1 || got[0] != "application/problem+json" {
		t.Fatalf("Content-Type=%v", got)
	}
	if got := w.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
		t.Fatalf("Cache-Control=%v", got)
	}
	records := waitForRecords(t, capture, 1)
	panicValue, ok := records[0].attrs["panic"].(string)
	if !ok || !strings.Contains(panicValue, "panic-marker") || strings.Contains(panicValue, secret) {
		t.Fatalf("panic log=%#v", records[0])
	}
}

func TestAPIRequestIDOnEveryStatus(t *testing.T) {
	h := NewAPIHandler([]APIRoute{{Method: "GET", Pattern: "/panic", Scope: "none", Handler: func(http.ResponseWriter, *http.Request) { panic("test") }}, {Method: "GET", Pattern: "/deny", Scope: "read", Handler: func(http.ResponseWriter, *http.Request) {}}, {Method: "GET", Pattern: "/ok", Scope: "none", Handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }}})
	for _, tc := range []struct {
		method, path string
		scopes       []string
		status       int
	}{{"GET", "/ok", nil, 200}, {"GET", "/deny", nil, 403}, {"GET", "/missing", nil, 404}, {"PUT", "/ok", nil, 405}, {"GET", "/panic", nil, 500}} {
		for _, incoming := range []string{"valid-id", "bad id!", strings.Repeat("x", 65), ""} {
			r := httptest.NewRequestWithContext(context.Background(), tc.method, "/api/v1"+tc.path, nil)
			if incoming != "" {
				r.Header.Set("X-Request-Id", incoming)
			}
			r = r.WithContext(WithPrincipal(r.Context(), Principal{Scopes: tc.scopes}))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			ids := w.Header().Values("X-Request-Id")
			if w.Code != tc.status || len(ids) != 1 {
				t.Fatalf("%s: status=%d ids=%v", tc.path, w.Code, ids)
			}
			wantContentType := "application/json"
			if tc.status != http.StatusOK {
				wantContentType = "application/problem+json"
			}
			if got := w.Header().Values("Content-Type"); len(got) != 1 || got[0] != wantContentType {
				t.Fatalf("%s: Content-Type=%v, want one %q", tc.path, got, wantContentType)
			}
			if got := w.Header().Values("Cache-Control"); len(got) != 1 || got[0] != "no-store" {
				t.Fatalf("%s: Cache-Control=%v", tc.path, got)
			}
			if incoming == "valid-id" {
				if ids[0] != incoming {
					t.Fatalf("id=%q", ids[0])
				}
			} else if len(ids[0]) != 32 || strings.Trim(ids[0], "0123456789abcdef") != "" {
				t.Fatalf("id=%q", ids[0])
			}
		}
	}
}

type apiCountingWriter struct {
	http.ResponseWriter
	writeHeaders int
}

func (w *apiCountingWriter) WriteHeader(status int) {
	w.writeHeaders++
	w.ResponseWriter.WriteHeader(status)
}

func TestAPIPanicAfterWriteKeepsResponse(t *testing.T) {
	secret := "rush_12345678." + strings.Repeat("A", 43)
	capture := &captureHandler{notify: make(chan struct{}, 1)}
	previous := slog.Default()
	slog.SetDefault(slog.New(capture))
	defer slog.SetDefault(previous)
	h := NewAPIHandler([]APIRoute{{Method: "GET", Pattern: "/panic-after-write", Scope: "none", Handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("partial response"))
		panic("panic-after-write-marker " + secret)
	}}})
	response := httptest.NewRecorder()
	w := &apiCountingWriter{ResponseWriter: response}
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), "GET", "/api/v1/panic-after-write", nil))
	if response.Code != http.StatusAccepted || response.Body.String() != "partial response" || w.writeHeaders != 1 {
		t.Fatalf("status=%d body=%q WriteHeader calls=%d", response.Code, response.Body.String(), w.writeHeaders)
	}
	records := waitForRecords(t, capture, 1)
	panicValue, ok := records[0].attrs["panic"].(string)
	if !ok || !strings.Contains(panicValue, "panic-after-write-marker") || strings.Contains(panicValue, secret) {
		t.Fatalf("panic log=%#v", records[0])
	}
}

func TestAPIRoutesPinned(t *testing.T) {
	got := APIRoutes()
	want := []APIRoute{
		{Method: "GET", Pattern: "/health", Scope: "none", OperationID: "getHealth"},
		{Method: "GET", Pattern: "/version", Scope: "read", OperationID: "getVersion"},
		{Method: "GET", Pattern: "/openapi.json", Scope: "read", OperationID: "getOpenAPI"},
		{Method: "GET", Pattern: "/sessions", Scope: "read", OperationID: "listSessions"},
		{Method: "GET", Pattern: "/sessions/{id}", Scope: "read", OperationID: "getSession"},
		{Method: "GET", Pattern: "/sessions/{id}/messages", Scope: "read", OperationID: "listMessages"},
		{Method: "GET", Pattern: "/models", Scope: "read", OperationID: "listModels"},
		{Method: "POST", Pattern: "/sessions", Scope: "run", OperationID: "createSession"},
		{Method: "POST", Pattern: "/sessions/{id}/fork", Scope: "run", OperationID: "forkSession"},
		{Method: "DELETE", Pattern: "/sessions/{id}", Scope: "run", OperationID: "deleteSession"},
		{Method: "POST", Pattern: "/sessions/{id}/runs", Scope: "run", OperationID: "createRun"},
		{Method: "GET", Pattern: "/runs/{id}", Scope: "read", OperationID: "getRun"},
		{Method: "POST", Pattern: "/runs/{id}/cancel", Scope: "run", OperationID: "cancelRun"},
		{Method: "POST", Pattern: "/sessions/{id}/inject", Scope: "run", OperationID: "injectMessage"},
	}
	if len(got) != len(want) {
		t.Fatalf("routes=%d", len(got))
	}
	for i, r := range want {
		if got[i].Method != r.Method || got[i].Pattern != r.Pattern || got[i].Scope != r.Scope || got[i].OperationID != r.OperationID {
			t.Fatalf("route[%d]=%+v want %+v", i, got[i], r)
		}
	}
}
