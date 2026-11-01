package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// TestInitializeSingleBoundsDiscoveryAfterConnect: a server that completes
// MCP initialization and then never answers tools/list must not hang full
// initialization; the unpublished preparation phase is bounded by the
// server's initialization timeout.
func TestInitializeSingleBoundsDiscoveryAfterConnect(t *testing.T) {
	const name = "discovery-stalled"
	var releaseOnce sync.Once
	release := make(chan struct{})
	stalled := mcp.NewServer(&mcp.Implementation{Name: "discovery-stalled"}, nil)
	mcp.AddTool(stalled, &mcp.Tool{Name: "stalled-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	stalled.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				select {
				case <-ctx.Done():
				case <-release:
				}
				return nil, context.Canceled
			}
			return next(ctx, method, request)
		}
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return stalled }, nil))
	cleanupTestMCPServer(t, stalled, httpServer)
	// Registered after the server cleanup so it runs first: a regressed
	// timeout must fail the test instead of wedging httptest.Server.Close.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	store := persistedMCPStore(t, name, httpServer.URL, false)
	_, ok := store.UpdateMCP(name, func(cfg *config.MCPConfig) { cfg.Timeout = 1 })
	require.True(t, ok)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, owner.Close(closeCtx))
	}()

	done := make(chan error, 1)
	go func() { done <- InitializeSingle(context.Background(), name, store) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		releaseOnce.Do(func() { close(release) })
		t.Fatal("initialization hung on a server that never answers tools/list")
	}
}
