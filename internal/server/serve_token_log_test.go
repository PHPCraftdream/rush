package server

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func setIsolatedGlobalPaths(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("RUSH_GLOBAL_DATA", base+"/data")
	t.Setenv("RUSH_GLOBAL_CONFIG", base+"/config")
	t.Setenv("RUSH_GLOBAL_DIR", base+"/dir")
}

func TestServeListenerDoesNotLogPlaintextToken(t *testing.T) {
	setIsolatedGlobalPaths(t)
	plainToken, _, err := config.GlobalAPITokenStore().CreateAPIToken("serve-log", []string{"run"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS}, Auth: DenyAllAuth}
	instance := NewWithPolicy(nil, policy, nil)
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", policy.Address)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- instance.ServeListener(ctx, listener, func(string) { ready <- struct{}{} }) }()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("ServeListener start: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("ServeListener did not become ready")
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+listener.Addr().String()+"/api/v1/health", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+plainToken)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("authenticated API request: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		cancel()
		t.Fatalf("authenticated API request status = %d, want 401 from fail-closed auth", response.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeListener shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ServeListener did not stop")
	}
	if strings.Contains(logs.String(), plainToken) {
		t.Fatalf("serve log contains plaintext API token: %q", logs.String())
	}
}

func TestWebListenerServeCycleDoesNotLogPlaintextToken(t *testing.T) {
	setIsolatedGlobalPaths(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	instance := New(nil, "127.0.0.1:0", nil)
	plainToken := instance.Token()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- instance.Start(ctx, func(addr string) { ready <- addr }) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("Start() = %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not start")
	}

	body := bytes.NewBufferString(`{"token":"` + plainToken + `"}`)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/auth", body)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("POST /auth: %v", err)
	}
	cookies := response.Cookies()
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(cookies) == 0 || cookies[0].Name != cookieName {
		cancel()
		t.Fatalf("POST /auth = %d, cookies=%v; want 200 and session cookie", response.StatusCode, cookies)
	}
	check, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/auth/check", nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	check.AddCookie(cookies[0])
	response, err = client.Do(check)
	if err != nil {
		cancel()
		t.Fatalf("GET /auth/check: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("GET /auth/check status = %d, want 200", response.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() shutdown = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
	if strings.Contains(logs.String(), plainToken) {
		t.Fatalf("web log contains plaintext UI token: %q", logs.String())
	}
}
