package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

type apiReadErrorStore struct {
	failMethod string
}

func (s apiReadErrorStore) ListSessions(context.Context) ([]session.Session, error) {
	if s.failMethod == "ListSessions" {
		return nil, apiReadStoreError()
	}
	return []session.Session{{ID: "known-session"}}, nil
}

func (s apiReadErrorStore) GetSession(context.Context, string) (session.Session, error) {
	if s.failMethod == "GetSession" {
		return session.Session{}, apiReadStoreError()
	}
	return session.Session{ID: "known-session"}, nil
}

func (s apiReadErrorStore) ListMessages(context.Context, string) ([]message.Message, error) {
	if s.failMethod == "ListMessages" {
		return nil, apiReadStoreError()
	}
	return []message.Message{}, nil
}

func (s apiReadErrorStore) ListModels(context.Context) ([]APIModel, error) {
	if s.failMethod == "ListModels" {
		return nil, apiReadStoreError()
	}
	return []APIModel{}, nil
}

func apiReadStoreError() error {
	return errors.New("backend failed token=rush_12345678.ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq /srv/private/customer-data")
}

func TestAPIReadStoreErrorsAreGeneric(t *testing.T) {
	const plaintextSecret = "rush_12345678.ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopq"
	const marker = "backend failed"
	const logMessage = "API request failed"
	const wantBody = "{\"type\":\"about:blank\",\"title\":\"Internal Server Error\",\"status\":500}\n"
	capture := &captureHandler{notify: make(chan struct{}, 4)}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: "ListSessions", path: "/api/v1/sessions"},
		{method: "GetSession", path: "/api/v1/sessions/known-session"},
		{method: "ListMessages", path: "/api/v1/sessions/known-session/messages"},
		{method: "ListModels", path: "/api/v1/models"},
	} {
		t.Run(tc.method, func(t *testing.T) {
			handler := NewAPIHandler(APIRoutesWithDeps(apiReadErrorStore{failMethod: tc.method}))
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, tc.path, nil)
			request = request.WithContext(WithPrincipal(request.Context(), Principal{Scopes: []string{"read"}}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusInternalServerError || response.Body.String() != wantBody {
				t.Fatalf("status=%d body=%q, want generic 500 body %q", response.Code, response.Body.String(), wantBody)
			}
			for _, forbidden := range []string{plaintextSecret, "backend failed", "/srv/private/customer-data"} {
				if strings.Contains(response.Body.String(), forbidden) {
					t.Errorf("response body contains %q: %s", forbidden, response.Body.String())
				}
			}
		})
	}

	records := waitForRecords(t, capture, 4)
	for _, record := range records {
		if record.message != logMessage {
			t.Errorf("log message=%q, want %q", record.message, logMessage)
		}
		if got, ok := record.attrs["error"].(string); !ok || !strings.Contains(got, marker) {
			t.Errorf("captured error=%#v, want marker %q", record.attrs["error"], marker)
		}
		if strings.Contains(record.attrs["error"].(string), plaintextSecret) {
			t.Errorf("captured log contains plaintext secret: %#v", record.attrs)
		}
	}
	if len(records) != 4 {
		t.Fatalf("captured %d log records, want 4", len(records))
	}
}

var _ APIReadStore = apiReadErrorStore{}
