package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteAPIResponseHeaders(t *testing.T) {
	tests := []struct {
		name        string
		requestID   string
		contentType string
		wantEcho    bool
	}{
		{name: "valid id", requestID: "Req_1.a-9", wantEcho: true},
		{name: "maximum id", requestID: strings.Repeat("a", 64), wantEcho: true},
		{name: "empty id generates", requestID: ""},
		{name: "too long generates", requestID: strings.Repeat("a", 65)},
		{name: "invalid character generates", requestID: "bad/id"},
		{name: "content type retained", requestID: "ok", contentType: "application/custom", wantEcho: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			r.Header.Set("X-Request-Id", tt.requestID)
			w := httptest.NewRecorder()
			if tt.contentType != "" {
				w.Header().Set("Content-Type", tt.contentType)
			}
			setAPIResponseHeaders(w, r)
			id := w.Header().Get("X-Request-Id")
			if tt.wantEcho && id != tt.requestID {
				t.Fatalf("request id = %q, want echoed %q", id, tt.requestID)
			}
			if !tt.wantEcho && (len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "") {
				t.Fatalf("generated request id = %q, want 32 lowercase hex bytes", id)
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q", got)
			}
			wantType := tt.contentType
			if wantType == "" {
				wantType = "application/json"
			}
			if got := w.Header().Get("Content-Type"); got != wantType {
				t.Fatalf("Content-Type = %q, want %q", got, wantType)
			}
		})
	}
	t.Run("nil request generates", func(t *testing.T) {
		w := httptest.NewRecorder()
		setAPIResponseHeaders(w, nil)
		if len(w.Header().Get("X-Request-Id")) != 32 {
			t.Fatalf("request id = %q", w.Header().Get("X-Request-Id"))
		}
	})
}

func TestSetAPIResponseHeadersKeepsExistingValidRequestID(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.Header.Set("X-Request-Id", "request-id")
	w := httptest.NewRecorder()
	w.Header().Add("X-Request-Id", "existing-id")
	setAPIResponseHeaders(w, r)
	setAPIResponseHeaders(w, r)
	if got := w.Header().Values("X-Request-Id"); len(got) != 1 || got[0] != "existing-id" {
		t.Fatalf("X-Request-Id = %v", got)
	}
}

func TestWriteAPIResponse(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.Header.Set("X-Request-Id", "req-7")
	w := httptest.NewRecorder()
	WriteAPIResponse(w, r, http.StatusCreated, map[string]string{"ok": "yes"})
	if w.Code != http.StatusCreated || w.Header().Get("X-Request-Id") != "req-7" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response status/headers: %d %#v", w.Code, w.Header())
	}
	if !bytes.Equal(w.Body.Bytes(), []byte("{\"ok\":\"yes\"}\n")) {
		t.Fatalf("body = %q", w.Body.Bytes())
	}
}

func TestWriteAPIProblem(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		title      string
		detail     string
		instance   string
		cause      error
		wantStatus int
		wantBody   APIProblem
	}{
		{name: "client problem", status: http.StatusBadRequest, title: "Bad Request", detail: "bad limit", instance: "/items", wantStatus: http.StatusBadRequest, wantBody: APIProblem{Type: "about:blank", Title: "Bad Request", Status: 400, Detail: "bad limit", Instance: "/items"}},
		{name: "generic internal with cause", status: http.StatusInternalServerError, title: "secret", detail: "secret", instance: "/private", cause: errors.New("secret cause"), wantStatus: http.StatusInternalServerError, wantBody: APIProblem{Type: "about:blank", Title: "Internal Server Error", Status: 500}},
		{name: "generic internal without cause", status: http.StatusServiceUnavailable, title: "unavailable", wantStatus: http.StatusServiceUnavailable, wantBody: APIProblem{Type: "about:blank", Title: "Internal Server Error", Status: http.StatusServiceUnavailable}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			WriteAPIProblem(w, r, tt.status, tt.title, tt.detail, tt.instance, tt.cause)
			if w.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", w.Code, tt.wantStatus)
			}
			if got := w.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Fatalf("Content-Type = %q", got)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-Id") == "" {
				t.Fatalf("missing common headers: %#v", w.Header())
			}
			var got APIProblem
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got != tt.wantBody {
				t.Fatalf("problem = %#v, want %#v", got, tt.wantBody)
			}
			if tt.status >= 500 && (strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "private")) {
				t.Fatalf("internal response disclosed cause/detail: %s", w.Body.String())
			}
		})
	}
}

func TestParsePagination(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		want    Pagination
		wantErr bool
	}{
		{name: "default", want: Pagination{Limit: 50}},
		{name: "cursor and maximum", query: "after=s_9&limit=200", want: Pagination{After: "s_9", Limit: 200}},
		{name: "minimum", query: "limit=1", want: Pagination{Limit: 1}},
		{name: "zero", query: "limit=0", wantErr: true},
		{name: "over maximum", query: "limit=201", wantErr: true},
		{name: "non numeric", query: "limit=many", wantErr: true},
		{name: "empty", query: "limit=", wantErr: true},
		{name: "duplicate", query: "limit=1&limit=2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items?"+tt.query, nil)
			got, err := ParsePagination(r)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("pagination = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestWritePaginatedResponse(t *testing.T) {
	tests := []struct {
		name    string
		ids     []string
		hasMore bool
		want    string
	}{
		{name: "empty last page", ids: nil, hasMore: false, want: "{\"items\":[],\"next_after\":null}\n"},
		{name: "empty page with more", ids: nil, hasMore: true, want: "{\"items\":[],\"next_after\":null}\n"},
		{name: "non-empty last page", ids: []string{"s1", "s2"}, hasMore: false, want: "{\"items\":[1,2],\"next_after\":null}\n"},
		{name: "cursor when more", ids: []string{"s1", "s2"}, hasMore: true, want: "{\"items\":[1,2],\"next_after\":\"s2\"}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			items := []int{}
			if len(tt.ids) > 0 {
				items = []int{1, 2}
			}
			WritePaginatedResponse(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/items", nil), http.StatusOK, items, tt.ids, tt.hasMore)
			if got := w.Body.String(); got != tt.want {
				t.Fatalf("body = %q, want %q", got, tt.want)
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("headers = %#v", w.Header())
			}
		})
	}
}
