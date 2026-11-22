package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func quiescentTestServer(t *testing.T, policy ListenerPolicy) (*Server, string, context.CancelFunc, <-chan error, <-chan struct{}) {
	t.Helper()
	setIsolatedGlobalPaths(t)
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	s := NewWithPolicy(nil, policy, nil)
	s.updateNotice = func(context.Context, *Hub) {}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		done <- s.Start(ctx, func(addr string) { ready <- addr })
		close(finished)
	}()
	select {
	case addr := <-ready:
		t.Cleanup(func() {
			cancel()
			select {
			case <-finished:
			case <-time.After(7 * time.Second):
				t.Error("server cleanup timed out")
			}
		})
		return s, addr, cancel, done, finished
	case err := <-done:
		cancel()
		t.Fatalf("Start: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("server did not start")
	}
	return nil, "", nil, nil, nil
}

func awaitTrackedState(t *testing.T, s *Server, state http.ConnState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.connMu.Lock()
		found := false
		for _, got := range s.connections {
			if got == state {
				found = true
				break
			}
		}
		s.connMu.Unlock()
		if found {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("connection in state %v was not registered", state)
}

func awaitEOF(t *testing.T, conn net.Conn, timeout time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	_, err := conn.Read(make([]byte, 1))
	if err == nil || (err != io.EOF && !strings.Contains(err.Error(), "closed")) {
		t.Fatalf("connection read = %v, want EOF/closed", err)
	}
}

func TestServeListenerShutdownClosesNeverUsedConnection(t *testing.T) {
	s, addr, cancel, done, finished := quiescentTestServer(t, ListenerPolicy{
		Address:  "127.0.0.1:0",
		Surfaces: []ListenerSurface{SurfaceREST},
		Auth:     DenyAllAuth,
	})
	client, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", addr)
	require.NoError(t, err)
	defer client.Close()
	awaitTrackedState(t, s, http.StateNew)
	started := time.Now()
	cancel()
	awaitEOF(t, client, 2*time.Second)
	select {
	case <-finished:
		require.NoError(t, <-done)
		require.Less(t, time.Since(started), 2*time.Second)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop promptly")
	}
}

func TestServeListenerShutdownKeepsActiveRequestUntilDone(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var logs synchronizedLogBuffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	auth := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/block" {
				close(entered)
				select {
				case <-release:
				case <-time.After(5 * time.Second):
					http.Error(w, "test handler timed out", http.StatusGatewayTimeout)
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, "complete")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	s, addr, cancel, done, finished := quiescentTestServer(t, ListenerPolicy{
		Address:  "127.0.0.1:0",
		Surfaces: []ListenerSurface{SurfaceREST},
		Auth:     auth,
	})
	responseDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/api/v1/block", nil)
		if err != nil {
			responseDone <- err
			return
		}
		request.Header.Set("Authorization", "Bearer quiescent-plaintext-sentinel")
		response, err := client.Do(request)
		if err != nil {
			responseDone <- err
			return
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil {
			responseDone <- readErr
			return
		}
		if response.StatusCode != http.StatusOK || string(body) != "complete" {
			responseDone <- errors.New("unexpected response status or body")
			return
		}
		responseDone <- nil
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	idle, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", addr)
	require.NoError(t, err)
	defer idle.Close()
	awaitTrackedState(t, s, http.StateNew)
	cancel()
	awaitEOF(t, idle, 2*time.Second)
	select {
	case err := <-responseDone:
		t.Fatalf("active response completed before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-finished:
		t.Fatalf("server returned before active request completed: %v", <-done)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-responseDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("active response did not complete")
	}
	select {
	case <-finished:
		require.NoError(t, <-done)
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop after active request completed")
	}
	require.NotContains(t, logs.String(), "quiescent-plaintext-sentinel")
}

func TestCloseQuiescentConnectionsOnlyClosesNewAndIdle(t *testing.T) {
	s := &Server{}
	states := []http.ConnState{http.StateNew, http.StateActive, http.StateIdle, http.StateHijacked, http.StateClosed}
	peers := make(map[http.ConnState]net.Conn, len(states))
	serverConns := make([]net.Conn, 0, len(states))
	for _, state := range states {
		serverConn, peer := net.Pipe()
		serverConns = append(serverConns, serverConn)
		peers[state] = peer
		s.trackConnection(serverConn, state)
	}
	defer func() {
		for _, conn := range serverConns {
			_ = conn.Close()
		}
		for _, conn := range peers {
			_ = conn.Close()
		}
	}()
	s.closeQuiescentConnections()
	for _, state := range states {
		peer := peers[state]
		wantClosed := state == http.StateNew || state == http.StateIdle
		// A closed net.Pipe rejects SetReadDeadline; Read reports EOF directly.
		if !wantClosed {
			if err := peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
				t.Fatal(err)
			}
		}
		_, err := peer.Read(make([]byte, 1))
		if wantClosed {
			if !errors.Is(err, io.EOF) && (err == nil || !strings.Contains(err.Error(), "closed")) {
				t.Errorf("state %v read error = %v, want closed", state, err)
			}
		} else {
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				t.Errorf("state %v read error = %v, want timeout", state, err)
			}
		}
	}
}
