package server

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

type sessionHandlerStore struct {
	item                     session.Session
	foreign, busy            bool
	deleted, created, forked int
	origin                   message.Origin
	maxBody                  int64
	createErr, forkErr       error
}

func (s *sessionHandlerStore) CreateSession(_ *http.Request, title string, origin message.Origin) (session.Session, error) {
	s.created++
	s.item = session.Session{ID: "created", Title: title, CreatedAt: 1700000000, UpdatedAt: 1700000001}
	s.origin = origin
	return s.item, s.createErr
}

func (s *sessionHandlerStore) ForkSession(_ *http.Request, id, title string) (session.Session, error) {
	s.forked++
	if s.forkErr != nil {
		return session.Session{}, s.forkErr
	}
	if id == "missing" {
		return session.Session{}, sql.ErrNoRows
	}
	return session.Session{ID: "forked", Title: title, CreatedAt: 1700000000, UpdatedAt: 1700000001}, nil
}

func (s *sessionHandlerStore) DeleteSession(_ *http.Request, _ string) error { s.deleted++; return nil }

func (s *sessionHandlerStore) GetSession(_ *http.Request, id string) (session.Session, error) {
	if s.item.ID == "" || id == "missing" {
		return session.Session{}, sql.ErrNoRows
	}
	return s.item, nil
}
func (s *sessionHandlerStore) SessionBusy(string) bool          { return s.busy }
func (s *sessionHandlerStore) OwnsSession(session.Session) bool { return !s.foreign }
func apiTestStoreHandler(store *sessionHandlerStore) http.Handler {
	routes := apiSessionWriteRoutes(store)
	return NewAPIHandler(routes)
}

func TestAPISessionWriteHandlers(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		body        string
		setup       func(*sessionHandlerStore)
		status      int
		response    string
		location    string
		errorTitle  string
		errorDetail string
		deleted     int
		created     int
		forked      int
		origin      message.Origin
		maxBody     int64
	}{
		{name: "create empty body", method: http.MethodPost, path: "/api/v1/sessions", status: http.StatusCreated, response: "{\"id\":\"created\",\"title\":\"New Session\",\"created_at\":\"2023-11-14T22:13:20Z\",\"updated_at\":\"2023-11-14T22:13:21Z\",\"message_count\":0}\n", location: "/api/v1/sessions/created", created: 1, origin: message.OriginAPI},
		{name: "unknown create field", method: http.MethodPost, path: "/api/v1/sessions", body: `{"unknown":true}`, status: http.StatusBadRequest, errorTitle: "Bad Request", errorDetail: "invalid request body"},
		{name: "fork", method: http.MethodPost, path: "/api/v1/sessions/source/fork", body: `{"title":"copy"}`, status: http.StatusCreated, response: "{\"id\":\"forked\",\"title\":\"copy\",\"created_at\":\"2023-11-14T22:13:20Z\",\"updated_at\":\"2023-11-14T22:13:21Z\",\"message_count\":0}\n", location: "/api/v1/sessions/forked", forked: 1},
		{name: "fork missing", method: http.MethodPost, path: "/api/v1/sessions/missing/fork", status: http.StatusNotFound, errorTitle: "Not Found", errorDetail: "session not found", forked: 1},
		{name: "delete", method: http.MethodDelete, path: "/api/v1/sessions/created", setup: func(s *sessionHandlerStore) { s.item = session.Session{ID: "created"} }, status: http.StatusNoContent, deleted: 1},
		{name: "delete missing", method: http.MethodDelete, path: "/api/v1/sessions/missing", status: http.StatusNotFound, errorTitle: "Not Found", errorDetail: "session not found"},
		{name: "delete busy", method: http.MethodDelete, path: "/api/v1/sessions/created", setup: func(s *sessionHandlerStore) { s.item = session.Session{ID: "created"}; s.busy = true }, status: http.StatusConflict, errorTitle: "Conflict", errorDetail: "session is busy"},
		{name: "delete foreign", method: http.MethodDelete, path: "/api/v1/sessions/created", setup: func(s *sessionHandlerStore) { s.item = session.Session{ID: "created"}; s.foreign = true }, status: http.StatusNotFound, errorTitle: "Not Found", errorDetail: "session not found"},
		{name: "oversize body", method: http.MethodPost, path: "/api/v1/sessions", body: `{"title":"long"}`, status: http.StatusRequestEntityTooLarge, maxBody: 3, errorTitle: "Request Entity Too Large", errorDetail: "request body too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &sessionHandlerStore{}
			if tt.setup != nil {
				tt.setup(store)
			}
			body := io.NopCloser(strings.NewReader(tt.body))
			r, err := http.NewRequestWithContext(t.Context(), tt.method, tt.path, body)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			if tt.maxBody > 0 {
				r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, tt.maxBody)
			}
			r = r.WithContext(WithPrincipal(r.Context(), Principal{Scopes: []string{"run"}}))
			w := httptest.NewRecorder()
			apiTestStoreHandler(store).ServeHTTP(w, r)
			result := w.Result()
			got, err := io.ReadAll(result.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := result.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if w.Code != tt.status || w.Header().Get("Location") != tt.location || store.deleted != tt.deleted || store.created != tt.created || store.forked != tt.forked || store.origin != tt.origin {
				t.Fatalf("status=%d body=%q location=%q deleted=%d created=%d forked=%d origin=%q", w.Code, got, w.Header().Get("Location"), store.deleted, store.created, store.forked, store.origin)
			}
			if tt.response != "" && string(got) != tt.response {
				t.Fatalf("body=%q want %q", got, tt.response)
			}
			if tt.errorTitle != "" {
				var problem APIProblem
				if err := json.Unmarshal(got, &problem); err != nil {
					t.Fatalf("decode problem: %v", err)
				}
				if problem.Status != tt.status || problem.Title != tt.errorTitle || problem.Detail != tt.errorDetail {
					t.Errorf("problem status=%d title=%q detail=%q; want status=%d title=%q detail=%q", problem.Status, problem.Title, problem.Detail, tt.status, tt.errorTitle, tt.errorDetail)
				}
			}
		})
	}
}

func TestAPISessionWriteScope(t *testing.T) {
	for _, route := range []struct{ method, path string }{{http.MethodPost, "/api/v1/sessions"}, {http.MethodPost, "/api/v1/sessions/source/fork"}, {http.MethodDelete, "/api/v1/sessions/source"}} {
		for _, scopes := range [][]string{{"read"}, {"run"}} {
			store := &sessionHandlerStore{item: session.Session{ID: "source"}}
			r, err := http.NewRequestWithContext(t.Context(), route.method, route.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Body.Close()
			r = r.WithContext(WithPrincipal(r.Context(), Principal{Scopes: scopes}))
			w := httptest.NewRecorder()
			apiTestStoreHandler(store).ServeHTTP(w, r)
			if scopes[0] == "read" && w.Code != http.StatusForbidden {
				t.Errorf("%s %s read status=%d", route.method, route.path, w.Code)
			}
			if scopes[0] == "run" && w.Code == http.StatusForbidden {
				t.Errorf("%s %s run denied", route.method, route.path)
			}
		}
	}
}
