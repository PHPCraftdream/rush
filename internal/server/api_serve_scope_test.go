package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

func TestServeAPIScopeDenialThroughProductionListener(t *testing.T) {
	isolateAPIGlobals(t)
	store := config.GlobalAPITokenStore()
	readToken, _, err := store.CreateAPIToken("scope-read", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	runToken, _, err := store.CreateAPIToken("scope-run", []string{"run"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	previousRoutes := serveAPIRoutes
	serveAPIRoutes = func(app *appPkg.App, broker *APIPermissionBroker, allowAutoApprove bool) ([]APIRoute, *APIEventLog, *APIRunRegistry) {
		routes, eventLog, registry := previousRoutes(app, broker, allowAutoApprove)
		return append(routes, APIRoute{Method: http.MethodGet, Pattern: "/scope-probe", Scope: "run", Handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }}), eventLog, registry
	}
	t.Cleanup(func() { serveAPIRoutes = previousRoutes })
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop API: %v", err)
		}
	})
	api.Configure(&policy)
	baseURL := startPolicyServer(t, NewWithPolicy(nil, policy, nil))
	client := &http.Client{Timeout: 5 * time.Second}
	request := func(token string) (serveAPIResult, []byte) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/scope-probe", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("read/close response: %v/%v", err, closeErr)
		}
		return serveAPIResult{StatusCode: resp.StatusCode, Header: resp.Header.Clone()}, body
	}
	denied, body := request(readToken)
	if denied.StatusCode != http.StatusForbidden || denied.Header.Get("Content-Type") != "application/problem+json" || string(body) != "{\"type\":\"about:blank\",\"title\":\"Forbidden\",\"status\":403,\"detail\":\"insufficient scope\"}\n" {
		t.Fatalf("read scope response = %d %q content-type=%q", denied.StatusCode, body, denied.Header.Get("Content-Type"))
	}
	allowed, body := request(runToken)
	if allowed.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("run scope response = %d %q, want 200 empty", allowed.StatusCode, body)
	}
	for _, token := range []string{readToken, runToken} {
		if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), token[strings.IndexByte(token, '.')+1:]) {
			t.Fatal("plaintext token appeared in captured logs")
		}
	}
}

// serveAPIResult is the part of a response the tests keep after the body has
// been read and closed inside the request helpers.
type serveAPIResult struct {
	StatusCode int
	Header     http.Header
}
