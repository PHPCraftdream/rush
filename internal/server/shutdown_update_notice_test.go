package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/update"
	"github.com/stretchr/testify/require"
)

type synchronizedLogBuffer struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (b *synchronizedLogBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.Write(data)
}

func (b *synchronizedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builder.String()
}

type shutdownBlockingUpdateClient struct {
	entered  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	returned chan struct{}
	once     sync.Once
}

func (c *shutdownBlockingUpdateClient) Latest(ctx context.Context) (*update.Release, error) {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-ctx.Done():
		close(c.canceled)
	case <-time.After(5 * time.Second):
		return nil, context.DeadlineExceeded
	}
	select {
	case <-c.release:
		close(c.returned)
	case <-time.After(5 * time.Second):
		close(c.returned)
		return nil, context.DeadlineExceeded
	}
	return nil, ctx.Err()
}

func TestServeListenerShutdownIgnoresBlockedUpdateAndIdleKeepAlive(t *testing.T) {
	setIsolatedGlobalPaths(t)
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	var logs synchronizedLogBuffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	updateClient := &shutdownBlockingUpdateClient{
		entered:  make(chan struct{}),
		canceled: make(chan struct{}),
		release:  make(chan struct{}),
		returned: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseClient := func() { releaseOnce.Do(func() { close(updateClient.release) }) }
	t.Cleanup(releaseClient)

	server := NewWithPolicy(nil, ListenerPolicy{
		Address:  "127.0.0.1:0",
		Surfaces: []ListenerSurface{SurfaceREST},
		Auth:     DenyAllAuth,
	}, nil)
	updateDone := make(chan struct{})
	server.updateNotice = func(ctx context.Context, hub *Hub) {
		defer close(updateDone)
		broadcastUpdateNoticeWithClientVersion(ctx, hub, updateClient, "1.0.0")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx, func(addr string) { ready <- addr }) }()
	var addr string
	select {
	case addr = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not become ready")
	}
	select {
	case <-updateClient.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("update check did not start")
	}

	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	idleConn := make(chan error, 1)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/api/v1/shutdown-log-sentinel", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer shutdown-plaintext-sentinel")
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
		PutIdleConn: func(err error) { idleConn <- err },
	}))
	response, err := client.Do(request)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	select {
	case err := <-idleConn:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP connection did not become idle")
	}

	startedAt := time.Now()
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
		require.Less(t, time.Since(startedAt), 2*time.Second)
	case <-time.After(2 * time.Second):
		releaseClient()
		t.Fatal("server shutdown waited for the update check or idle connection")
	}
	select {
	case <-updateClient.canceled:
	case <-time.After(2 * time.Second):
		releaseClient()
		t.Fatal("update check did not observe cancellation")
	}
	releaseClient()
	select {
	case <-updateClient.returned:
	case <-time.After(2 * time.Second):
		t.Fatal("update check did not return after release")
	}
	select {
	case <-updateDone:
	case <-time.After(2 * time.Second):
		t.Fatal("update notice did not finish")
	}
	logText := logs.String()
	require.NotContains(t, logText, "shutdown-plaintext-sentinel")
}
