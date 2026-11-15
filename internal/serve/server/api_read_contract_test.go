package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/session"
)

func TestAPIReadGetSessionExactJSON(t *testing.T) {
	fixture := apiReadFixture{sessions: []session.Session{{
		ID: "session-fixed", Title: "A fixed session", CreatedAt: 1_700_000_000,
		UpdatedAt: 1_700_000_123, MessageCount: 7,
	}}}
	handler := NewAPIHandler(APIRoutesWithDeps(fixture))
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/sessions/session-fixed", nil)
	request = request.WithContext(WithPrincipal(request.Context(), Principal{Scopes: []string{"read"}}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	const want = "{\"id\":\"session-fixed\",\"title\":\"A fixed session\",\"created_at\":\"2023-11-14T22:13:20Z\",\"updated_at\":\"2023-11-14T22:15:23Z\",\"message_count\":7}\n"
	if response.Code != http.StatusOK || response.Body.String() != want {
		t.Fatalf("status=%d body=%q want=%q", response.Code, response.Body.String(), want)
	}
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", got)
	}
	if got := response.Header().Get("X-Request-Id"); got == "" {
		t.Error("X-Request-Id is absent")
	}
}

func TestAPIReadProblemShape(t *testing.T) {
	fixture := apiReadFixture{
		sessions: []session.Session{{ID: "owned-session", UpdatedAt: 1}},
	}
	handler := NewAPIHandler(APIRoutesWithDeps(fixture))
	const requestID = "read-problem-test-01"
	cases := []struct {
		name       string
		path       string
		status     int
		title      string
		detail     string
		noIDDetail bool
	}{
		{name: "invalid sessions limit", path: "/api/v1/sessions?limit=0", status: http.StatusBadRequest, title: "Bad Request", detail: "limit must be an integer from 1 to 200"},
		{name: "unknown sessions cursor", path: "/api/v1/sessions?after=unknown", status: http.StatusBadRequest, title: "Bad Request", detail: "after cursor not found"},
		{name: "invalid messages limit", path: "/api/v1/sessions/owned-session/messages?limit=201", status: http.StatusBadRequest, title: "Bad Request", detail: "limit must be an integer from 1 to 200"},
		{name: "foreign messages cursor", path: "/api/v1/sessions/owned-session/messages?after=foreign-message", status: http.StatusBadRequest, title: "Bad Request", detail: "after cursor not found"},
		{name: "missing session endpoint", path: "/api/v1/sessions/requested-session-id", status: http.StatusNotFound, title: "Not Found", detail: "session not found", noIDDetail: true},
		{name: "missing session messages", path: "/api/v1/sessions/requested-session-id/messages", status: http.StatusNotFound, title: "Not Found", detail: "session not found", noIDDetail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
			request.Header.Set("X-Request-Id", requestID)
			request = request.WithContext(WithPrincipal(request.Context(), Principal{Scopes: []string{"read"}}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.status, response.Body.String())
			}
			if got := response.Header().Get("Content-Type"); got != "application/problem+json" {
				t.Fatalf("Content-Type=%q want application/problem+json", got)
			}
			if got := response.Header().Get("X-Request-Id"); got != requestID {
				t.Errorf("X-Request-Id=%q want=%q", got, requestID)
			}
			var problem APIProblem
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatalf("decode problem: %v", err)
			}
			if problem.Type != "about:blank" || problem.Title != tc.title || problem.Status != tc.status || problem.Detail != tc.detail {
				t.Errorf("problem=%+v want type=about:blank title=%q status=%d detail=%q", problem, tc.title, tc.status, tc.detail)
			}
			if tc.noIDDetail && strings.Contains(problem.Detail, "requested-session-id") {
				t.Errorf("404 detail contains requested ID: %q", problem.Detail)
			}
		})
	}
}

func TestAPIReadEmptyAndOrdering(t *testing.T) {
	t.Run("empty sessions", func(t *testing.T) {
		handler := NewAPIHandler(APIRoutesWithDeps(apiReadFixture{}))
		response := apiReadRequest(t, handler, "/api/v1/sessions", []string{"read"})
		const want = "{\"items\":[],\"next_after\":null}\n"
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("status=%d body=%q want=%q", response.Code, response.Body.String(), want)
		}
	})
	t.Run("empty messages", func(t *testing.T) {
		fixture := apiReadFixture{sessions: []session.Session{{ID: "empty-session"}}}
		handler := NewAPIHandler(APIRoutesWithDeps(fixture))
		response := apiReadRequest(t, handler, "/api/v1/sessions/empty-session/messages", []string{"read"})
		const want = "{\"items\":[],\"next_after\":null}\n"
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("status=%d body=%q want=%q", response.Code, response.Body.String(), want)
		}
	})
	t.Run("timestamp order with ID tie break", func(t *testing.T) {
		fixture := apiReadFixture{sessions: []session.Session{
			{ID: "equal-z", Title: "equal z", CreatedAt: 10, UpdatedAt: 20},
			{ID: "older", Title: "older", CreatedAt: 1, UpdatedAt: 10},
			{ID: "equal-a", Title: "equal a", CreatedAt: 9, UpdatedAt: 20},
			{ID: "newest", Title: "newest", CreatedAt: 2, UpdatedAt: 30},
		}}
		handler := NewAPIHandler(APIRoutesWithDeps(fixture))
		response := apiReadRequest(t, handler, "/api/v1/sessions", []string{"read"})
		const want = "{\"items\":[{\"id\":\"newest\",\"title\":\"newest\",\"created_at\":\"1970-01-01T00:00:02Z\",\"updated_at\":\"1970-01-01T00:00:30Z\",\"message_count\":0},{\"id\":\"equal-a\",\"title\":\"equal a\",\"created_at\":\"1970-01-01T00:00:09Z\",\"updated_at\":\"1970-01-01T00:00:20Z\",\"message_count\":0},{\"id\":\"equal-z\",\"title\":\"equal z\",\"created_at\":\"1970-01-01T00:00:10Z\",\"updated_at\":\"1970-01-01T00:00:20Z\",\"message_count\":0},{\"id\":\"older\",\"title\":\"older\",\"created_at\":\"1970-01-01T00:00:01Z\",\"updated_at\":\"1970-01-01T00:00:10Z\",\"message_count\":0}],\"next_after\":null}\n"
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("status=%d body=%q want=%q", response.Code, response.Body.String(), want)
		}
	})
}
