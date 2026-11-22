package server

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func startPolicyServer(t *testing.T, s *Server) string {
	t.Helper()
	tl := &panicTimeline{}
	tl.add("Start scheduled")
	t.Cleanup(func() { reportListenerFailure(t, tl, func() string { return listenerSnapshot(s) }) })
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		tl.add("Start entered")
		err := s.Start(ctx, func(addr string) {
			tl.add("listener ready")
			ready <- addr
		})
		tl.add("Start returned")
		errCh <- err
	}()
	select {
	case addr := <-ready:
		t.Cleanup(func() {
			defer reportListenerFailure(t, tl, func() string { return listenerSnapshot(s) })
			tl.add("cancel requested")
			cancel()
			select {
			case err := <-errCh:
				if err != nil {
					t.Errorf("server stopped: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("server did not shut down within 5s of cancel")
			}
		})
		return "http://" + addr
	case err := <-errCh:
		cancel()
		t.Fatalf("Start() error = %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not become ready")
	}
	return ""
}

func TestServeListener_NoUISurfaceNoUIToken(t *testing.T) {
	policy := ListenerPolicy{
		Address:  "127.0.0.1:0",
		Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS},
		Auth:     DenyAllAuth,
	}
	server := NewWithPolicy(nil, policy, nil)
	baseURL := startPolicyServer(t, server)
	if got := server.Token(); got != "" {
		t.Fatalf("Token() = %q, want empty", got)
	}
	for _, path := range []string{"/", "/auth", "/auth/check"} {
		request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+path, nil)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s status = %d, want 404 with no UI route", path, response.StatusCode)
		}
	}
	for _, path := range []string{"/ws", "/ws?token=arbitrary"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(&http.Cookie{Name: cookieName, Value: "arbitrary"})
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want 401", path, response.StatusCode)
		}
		if response.Header.Get("WWW-Authenticate") != "Bearer" || response.ContentLength != 0 {
			t.Errorf("GET %s auth response headers/body = %q/%d, want Bearer/empty", path, response.Header.Get("WWW-Authenticate"), response.ContentLength)
		}
	}
}

func TestWebListenerStillServesSPAAndAuth(t *testing.T) {
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>web-ui</html>")}}
	server := New(nil, "127.0.0.1:0", fs.FS(static))
	baseURL := startPolicyServer(t, server)
	request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/", nil)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr != nil {
		t.Fatalf("read SPA response: %v", readErr)
	}
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "web-ui") {
		t.Fatalf("GET / = %d %q, want SPA", response.StatusCode, body)
	}
	request, requestErr = http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL+"/auth", nil)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /auth status = %d, want 405", response.StatusCode)
	}
}

func TestServeListener_TLSPolicyPlaceholderDoesNotPermitCleartext(t *testing.T) {
	policy := ListenerPolicy{
		Address: "0.0.0.0:8080", Surfaces: []ListenerSurface{SurfaceREST},
		TLS: ListenerTLS{Cert: "cert.pem", Key: "key.pem"}, Auth: DenyAllAuth,
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("configured TLS policy rejected: %v", err)
	}
}
