package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIMuxPassesPathWildcards(t *testing.T) {
	var got []string
	handler := NewAPIHandler([]APIRoute{{
		Method: "GET", Pattern: "/sessions/{id}/messages/{mid}", Scope: "none",
		Handler: func(w http.ResponseWriter, r *http.Request) {
			got = append(got, r.PathValue("id"), r.PathValue("mid"))
			w.WriteHeader(http.StatusNoContent)
		},
	}})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/sessions/s-1/messages/m-2", nil))
	if res.Code != http.StatusNoContent || len(got) != 2 || got[0] != "s-1" || got[1] != "m-2" {
		t.Fatalf("status %d, path values %q", res.Code, got)
	}
}

func TestAPIMuxWildcardRouteStill405And404(t *testing.T) {
	handler := NewAPIHandler([]APIRoute{{
		Method: "GET", Pattern: "/things/{id}", Scope: "none",
		Handler: func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) },
	}})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/api/v1/things/7", nil))
	if res.Code != http.StatusMethodNotAllowed || res.Header().Get("Allow") != "GET" {
		t.Fatalf("405 case: %d allow %q", res.Code, res.Header().Get("Allow"))
	}
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/things", nil))
	if res.Code != http.StatusNotFound {
		t.Fatalf("404 case: %d", res.Code)
	}
}

func TestAPIMuxResponseControllerReachesFlusher(t *testing.T) {
	handler := NewAPIHandler([]APIRoute{{
		Method: "GET", Pattern: "/stream", Scope: "none",
		Handler: func(w http.ResponseWriter, r *http.Request) {
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flush through the API writer: %v", err)
			}
		},
	}})
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/stream", nil))
	if !res.Flushed {
		t.Fatal("underlying writer was not flushed")
	}
}
