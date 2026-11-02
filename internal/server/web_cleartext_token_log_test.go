package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestWebListenerDoesNotLogPlaintextToken(t *testing.T) {
	setIsolatedGlobalPaths(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	s := New(nil, "127.0.0.1:0", nil)
	token := s.Token()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx, func(addr string) { ready <- addr }) }()
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

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+addr+"/auth", bytes.NewBufferString(`{"token":"`+token+`"}`))
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
			t.Fatalf("Start() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("server log contains plaintext UI token: %q", logs.String())
	}
}
