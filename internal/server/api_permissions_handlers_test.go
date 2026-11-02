package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

func permissionHandlerRequest(t *testing.T, handler http.Handler, method, path, body string, principal Principal) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.SetPathValue("id", "p1")
	r = r.WithContext(WithPrincipal(r.Context(), principal))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestAPIPermissionHandlersAnswerAndList(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", filepath.Join(t.TempDir(), "config"))
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	service := newAPIPermissionTestService()
	ctx, cancel := context.WithCancel(context.Background())
	broker := NewAPIPermissionBroker(service, "", func(event apiPermissionEvent) { service.notifications <- event }, time.Hour)
	broker.newTimer = func(time.Duration) apiPermissionTimer { return newAPIPermissionTestTimer() }
	if err := broker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); broker.Stop() }()
	if err := broker.Register("run", "session", "owner"); err != nil {
		t.Fatal(err)
	}
	service.events <- pubsub.Event[permission.PermissionRequest]{Payload: permission.PermissionRequest{ID: "p1", SessionID: "session"}}
	select {
	case <-service.notifications:
	case <-time.After(10 * time.Second):
		t.Fatal("request not accepted")
	}
	handler := NewAPIHandler(apiPermissionRoutes(broker))
	owner := Principal{TokenID: "owner", Scopes: []string{"run"}}
	if got := permissionHandlerRequest(t, handler, http.MethodGet, "/api/v1/permissions?session=session", "", owner); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"id":"p1"`) {
		t.Fatalf("list status=%d body=%s", got.Code, got.Body.String())
	}
	if got := permissionHandlerRequest(t, handler, http.MethodPost, "/api/v1/permissions/p1", `{"decision":"allow_session"}`, owner); got.Code != http.StatusBadRequest || got.Body.String() != "{\"type\":\"about:blank\",\"title\":\"Bad Request\",\"status\":400,\"detail\":\"invalid permission decision\"}\n" {
		t.Fatalf("invalid status=%d body=%s", got.Code, got.Body.String())
	}
	if got := permissionHandlerRequest(t, handler, http.MethodPost, "/api/v1/permissions/p1", `{"decision":"allow"}`, Principal{TokenID: "other", Scopes: []string{"run"}}); got.Code != http.StatusNotFound || got.Body.String() != "{\"type\":\"about:blank\",\"title\":\"Not Found\",\"status\":404,\"detail\":\"permission not found\"}\n" {
		t.Fatalf("foreign status=%d body=%s", got.Code, got.Body.String())
	}
	if got := permissionHandlerRequest(t, handler, http.MethodPost, "/api/v1/permissions/p1", `{"decision":"allow"}`, owner); got.Code != http.StatusOK {
		t.Fatalf("answer status=%d body=%s", got.Code, got.Body.String())
	}
	service.mu.Lock()
	grants, persistent := service.grants, service.persistent
	service.mu.Unlock()
	if grants != 1 || persistent != 0 {
		t.Fatalf("grants=%d persistent=%d", grants, persistent)
	}
	if got := permissionHandlerRequest(t, handler, http.MethodPost, "/api/v1/permissions/p1", `{"decision":"deny"}`, owner); got.Code != http.StatusConflict || got.Body.String() != "{\"type\":\"about:blank\",\"title\":\"Conflict\",\"status\":409,\"detail\":\"permission already decided\"}\n" {
		t.Fatalf("duplicate status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestAPIPermissionRoutesOpenAPI(t *testing.T) {
	want := map[string]string{"GET /permissions": "listPermissions", "POST /permissions/{id}": "answerPermission"}
	for _, route := range APIRoutes() {
		key := route.Method + " " + route.Pattern
		if operation, ok := want[key]; ok {
			if route.OperationID != operation || route.Handler == nil {
				t.Errorf("%s operation=%q", key, route.OperationID)
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing routes: %v", want)
	}
	document := BuildOpenAPIDocument(APIRoutes())
	paths := document["paths"].(map[string]any)
	if paths["/permissions"] == nil || paths["/permissions/{id}"] == nil {
		t.Fatal("permission paths absent from OpenAPI")
	}
}
