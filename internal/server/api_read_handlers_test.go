package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

type apiReadFixture struct {
	sessions []session.Session
	messages []message.Message
	models   []APIModel
}

func (f apiReadFixture) ListSessions(context.Context) ([]session.Session, error) {
	return append([]session.Session(nil), f.sessions...), nil
}

func (f apiReadFixture) GetSession(_ context.Context, id string) (session.Session, error) {
	for _, item := range f.sessions {
		if item.ID == id {
			return item, nil
		}
	}
	return session.Session{}, sql.ErrNoRows
}

func (f apiReadFixture) ListMessages(context.Context, string) ([]message.Message, error) {
	return append([]message.Message(nil), f.messages...), nil
}

func (f apiReadFixture) ListModels(context.Context) ([]APIModel, error) {
	return append([]APIModel(nil), f.models...), nil
}

func apiReadRequest(t *testing.T, handler http.Handler, path string, scopes []string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	r = r.WithContext(WithPrincipal(r.Context(), Principal{Scopes: scopes}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestAPIReadSessionsProjectionAndPagination(t *testing.T) {
	fixture := apiReadFixture{sessions: []session.Session{{ID: "s-1", Title: "safe", CreatedAt: 0, UpdatedAt: 10, MessageCount: 2, WorkspaceRoot: "/secret/cwd", SystemPrompt: "provider-key-secret"}}}
	h := NewAPIHandler(APIRoutesWithDeps(fixture))
	w := apiReadRequest(t, h, "/api/v1/sessions", []string{"read"})
	const want = "{\"items\":[{\"id\":\"s-1\",\"title\":\"safe\",\"created_at\":\"1970-01-01T00:00:00Z\",\"updated_at\":\"1970-01-01T00:00:10Z\",\"message_count\":2}],\"next_after\":null}\n"
	if w.Code != http.StatusOK || w.Body.String() != want || strings.Contains(w.Body.String(), "/secret/cwd") || strings.Contains(w.Body.String(), "provider-key-secret") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAPIReadPaginationThreePagesAndValidation(t *testing.T) {
	fixture := apiReadFixture{}
	for i := 0; i < 120; i++ {
		fixture.sessions = append(fixture.sessions, session.Session{ID: fmt.Sprintf("s-%03d", i), UpdatedAt: int64(120 - i)})
	}
	h := NewAPIHandler(APIRoutesWithDeps(fixture))
	for page, after := range []string{"", "s-049", "s-099"} {
		path := "/api/v1/sessions?limit=50"
		if after != "" {
			path += "&after=" + after
		}
		w := apiReadRequest(t, h, path, []string{"read"})
		var got struct {
			Items     []APISession `json:"items"`
			NextAfter any          `json:"next_after"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
			t.Fatalf("page=%d status=%d body=%s", page, w.Code, w.Body.String())
		}
		wantCount := 50
		if page == 2 {
			wantCount = 20
		}
		if len(got.Items) != wantCount {
			t.Fatalf("page=%d items=%d want=%d", page, len(got.Items), wantCount)
		}
		for i, item := range got.Items {
			wantID := fmt.Sprintf("s-%03d", page*50+i)
			if item.ID != wantID {
				t.Fatalf("page=%d item[%d]=%q want %q", page, i, item.ID, wantID)
			}
		}
		if page < 2 && got.NextAfter != got.Items[len(got.Items)-1].ID {
			t.Fatalf("page=%d next=%v", page, got.NextAfter)
		}
		if page == 2 && got.NextAfter != nil {
			t.Fatalf("final exact page next=%v", got.NextAfter)
		}
	}
	defaultPage := apiReadRequest(t, h, "/api/v1/sessions", []string{"read"})
	var defaultResult struct {
		Items     []APISession `json:"items"`
		NextAfter any          `json:"next_after"`
	}
	if defaultPage.Code != http.StatusOK || json.Unmarshal(defaultPage.Body.Bytes(), &defaultResult) != nil || len(defaultResult.Items) != 50 || defaultResult.NextAfter != defaultResult.Items[49].ID {
		t.Fatalf("default page status=%d body=%s", defaultPage.Code, defaultPage.Body.String())
	}
	maxPage := apiReadRequest(t, h, "/api/v1/sessions?limit=200", []string{"read"})
	var maxResult struct {
		Items []APISession `json:"items"`
	}
	if maxPage.Code != http.StatusOK || json.Unmarshal(maxPage.Body.Bytes(), &maxResult) != nil || len(maxResult.Items) != 120 {
		t.Fatalf("maximum page status=%d body=%s", maxPage.Code, maxPage.Body.String())
	}
	exactFixture := apiReadFixture{}
	for i := 0; i < 100; i++ {
		exactFixture.sessions = append(exactFixture.sessions, session.Session{ID: fmt.Sprintf("x-%03d", i), UpdatedAt: int64(100 - i)})
	}
	exactHandler := NewAPIHandler(APIRoutesWithDeps(exactFixture))
	exactFinal := apiReadRequest(t, exactHandler, "/api/v1/sessions?limit=50&after=x-049", []string{"read"})
	var exactResult struct {
		Items     []APISession `json:"items"`
		NextAfter any          `json:"next_after"`
	}
	if exactFinal.Code != http.StatusOK || json.Unmarshal(exactFinal.Body.Bytes(), &exactResult) != nil || len(exactResult.Items) != 50 || exactResult.NextAfter != nil {
		t.Fatalf("exact-multiple final page status=%d body=%s", exactFinal.Code, exactFinal.Body.String())
	}
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=nope", "?limit=1&limit=2"} {
		w := apiReadRequest(t, h, "/api/v1/sessions"+query, []string{"read"})
		if w.Code != http.StatusBadRequest {
			t.Errorf("query=%q status=%d", query, w.Code)
		}
	}
	w := apiReadRequest(t, h, "/api/v1/sessions?after=missing", []string{"read"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown cursor status=%d", w.Code)
	}
}

func TestAPIReadMessagesProjectionAndCursor(t *testing.T) {
	fixture := apiReadFixture{
		sessions: []session.Session{{ID: "s"}},
		messages: []message.Message{
			{ID: "m-1", SessionID: "s", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hello"}}, CreatedAt: 1},
			{ID: "m-2", SessionID: "s", Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "call", Name: "tool", Input: "{}"}}, CreatedAt: 2},
		},
	}
	h := NewAPIHandler(APIRoutesWithDeps(fixture))
	w := apiReadRequest(t, h, "/api/v1/sessions/s/messages?limit=1", []string{"read"})
	if w.Code != http.StatusOK || w.Body.String() != "{\"items\":[{\"id\":\"m-1\",\"session_id\":\"s\",\"role\":\"user\",\"parts\":[{\"type\":\"text\",\"text\":\"hello\"}],\"created_at\":\"1970-01-01T00:00:01Z\"}],\"next_after\":\"m-1\"}\n" {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	w = apiReadRequest(t, h, "/api/v1/sessions/s/messages?after=m-1", []string{"read"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"m-2"`) || strings.Contains(w.Body.String(), `"id":"m-1"`) {
		t.Fatalf("cursor status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAPIReadNotFoundModelsAndScope(t *testing.T) {
	fixture := apiReadFixture{models: []APIModel{{ID: "model-id", Name: "Model", Provider: "provider"}}}
	h := NewAPIHandler(APIRoutesWithDeps(fixture))
	for _, path := range []string{"/api/v1/sessions/missing", "/api/v1/sessions/missing/messages"} {
		w := apiReadRequest(t, h, path, []string{"read"})
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "missing") {
			t.Errorf("path=%s status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	w := apiReadRequest(t, h, "/api/v1/models", []string{"read"})
	if w.Code != http.StatusOK || w.Body.String() != "[{\"id\":\"model-id\",\"name\":\"Model\",\"provider\":\"provider\"}]\n" {
		t.Fatalf("models status=%d body=%s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/api/v1/sessions", "/api/v1/sessions/missing", "/api/v1/sessions/missing/messages", "/api/v1/models"} {
		w := apiReadRequest(t, h, path, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("path=%s status=%d", path, w.Code)
		}
	}
}

func TestAPIReadModelsDoNotProjectSecrets(t *testing.T) {
	const secret = "provider-key-secret"
	workingDir := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	a := newAttachmentsTestApp(t, workingDir, t.TempDir())
	a.Store().SetProviderRuntimeConfig("fixture", config.ProviderConfig{
		ID: "fixture", Name: "Fixture", Type: catwalk.TypeOpenAICompat, APIKey: secret,
		Models: []catwalk.Model{{ID: "safe-model", Name: "Safe model"}},
	})
	w := apiReadRequest(t, NewAPIHandler(APIRoutesWithApp(a)), "/api/v1/models", []string{"read"})
	body := w.Body.String()
	for _, unsafe := range []string{
		secret,
		escapedJSONValue(t, secret),
		workingDir,
		escapedJSONValue(t, workingDir),
		filepath.ToSlash(workingDir),
		escapedJSONValue(t, filepath.ToSlash(workingDir)),
	} {
		if strings.Contains(body, unsafe) {
			t.Fatalf("models response contains sensitive value %q: %s", unsafe, body)
		}
	}
	if w.Code != http.StatusOK || !strings.Contains(body, "safe-model") {
		t.Fatalf("status=%d unsafe body=%s", w.Code, body)
	}
}

func TestAPIReadProductionAppSmoke(t *testing.T) {
	dataDir := t.TempDir()
	workingDir := t.TempDir()
	isolateAPIGlobals(t)
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "read smoke")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	api := NewAPIHandler(APIRoutesWithApp(a))
	list := apiReadRequest(t, api, "/api/v1/sessions", []string{"read"})
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"id":"`+sess.ID+`"`) || !strings.Contains(list.Body.String(), `"title":"read smoke"`) {
		t.Fatalf("sessions status=%d body=%s", list.Code, list.Body.String())
	}
	messages := apiReadRequest(t, api, "/api/v1/sessions/"+sess.ID+"/messages", []string{"read"})
	if messages.Code != http.StatusOK || !strings.Contains(messages.Body.String(), `"session_id":"`+sess.ID+`"`) || !strings.Contains(messages.Body.String(), `"text":"hello"`) {
		t.Fatalf("messages status=%d body=%s", messages.Code, messages.Body.String())
	}
}

func TestAPIReadProductionWiringIntegration(t *testing.T) {
	const plaintextToken = "known-plaintext-token-must-not-be-logged"
	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	fixture := apiReadFixture{sessions: []session.Session{{ID: "production", Title: "wired"}}, models: []APIModel{}}
	api := NewAPIHandler(APIRoutesWithDeps(fixture))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := Principal{}
		if r.Header.Get("X-Test-Scopes") == "read" {
			principal.Scopes = []string{"read"}
		}
		api.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
	}))
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 10 * time.Second
	for _, tc := range []struct {
		path   string
		scoped bool
		status int
	}{{"/api/v1/sessions", true, http.StatusOK}, {"/api/v1/sessions/production", true, http.StatusOK}, {"/api/v1/sessions/production/messages", true, http.StatusOK}, {"/api/v1/models", true, http.StatusOK}, {"/api/v1/sessions", false, http.StatusForbidden}, {"/api/v1/sessions/production", false, http.StatusForbidden}, {"/api/v1/sessions/production/messages", false, http.StatusForbidden}, {"/api/v1/models", false, http.StatusForbidden}} {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+tc.path, nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if tc.scoped {
			req.Header.Set("X-Test-Scopes", "read")
			req.Header.Set("Authorization", "Bearer "+plaintextToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		_, readErr := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		cancel()
		if readErr != nil || closeErr != nil || resp.StatusCode != tc.status {
			t.Errorf("%s status=%d read=%v close=%v", tc.path, resp.StatusCode, readErr, closeErr)
		}
	}
	if strings.Contains(logs.String(), plaintextToken) {
		t.Fatal("integration logs contain plaintext token")
	}
}

func TestAPIReadCursorStart(t *testing.T) {
	items := []string{"a", "b", "c"}
	for _, tc := range []struct {
		after string
		want  int
		ok    bool
	}{{"", 0, true}, {"a", 1, true}, {"c", 3, true}, {"missing", 0, false}} {
		got, ok := cursorStart(items, tc.after, func(value string) string { return value })
		if got != tc.want || ok != tc.ok {
			t.Errorf("after=%q got=(%d,%v), want=(%d,%v)", tc.after, got, ok, tc.want, tc.ok)
		}
	}
}
