package sdk_test

// The application and library clients deliberately share the process-wide
// MCP registry, but only the application ConfigStore owns its entries. This
// is an end-to-end regression for the three model-facing surfaces: the turn
// prompt, tool schemas, and rush_info output.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalmcp "github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/sdk"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestApplicationMCPDoesNotEnterConcurrentLibrarySurface(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const (
		serverName     = "application-only-mcp"
		toolName       = "application-secret-tool"
		instructions   = "APPLICATION_ONLY_MCP_INSTRUCTIONS"
		marker         = "LIBRARY_MCP_ISOLATION_OK"
		rushInfoCallID = "library-rush-info"
	)

	mcpServer := modelmcp.NewServer(
		&modelmcp.Implementation{Name: serverName},
		&modelmcp.ServerOptions{Instructions: instructions},
	)
	modelmcp.AddTool(mcpServer, &modelmcp.Tool{
		Name:        toolName,
		Description: "APPLICATION_ONLY_MCP_TOOL_SCHEMA",
	}, func(context.Context, *modelmcp.CallToolRequest, any) (*modelmcp.CallToolResult, any, error) {
		return &modelmcp.CallToolResult{}, nil, nil
	})
	mcpHTTP := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(
		func(*http.Request) *modelmcp.Server { return mcpServer }, nil,
	))
	t.Cleanup(mcpHTTP.Close)

	provider := newR17MCPIsolationProvider(t, marker, rushInfoCallID)
	applicationDir := t.TempDir()
	applicationConfig := map[string]any{
		"disable_default_providers": true,
		"providers": map[string]any{
			"probe": map[string]any{
				"id": "probe", "name": "probe", "type": "openai-compat",
				"base_url": provider.URL, "api_key": "probe", "discover_models": false,
				"models": []any{map[string]any{
					"id": "probe", "name": "probe", "context_window": 200000,
					"default_max_tokens": 1000,
				}},
			},
		},
		"models": map[string]any{
			"smart": map[string]string{"provider": "probe", "model": "probe"},
			"fast":  map[string]string{"provider": "probe", "model": "probe"},
		},
		"mcp": map[string]any{
			serverName: map[string]any{
				"type": "http", "url": mcpHTTP.URL, "enabled_in_cli": true,
			},
		},
	}
	configData, err := json.Marshal(applicationConfig)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(applicationDir, "rush.json"), configData, 0o600))

	application, err := sdk.Open(context.Background(), sdk.Options{
		WorkingDir: applicationDir,
		MCP:        sdk.MCPAll,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = application.Close() })

	require.Eventually(t, func() bool {
		state, ok := internalmcp.GetState(serverName)
		return ok && state.State == internalmcp.StateConnected && state.Counts.Tools == 1
	}, 10*time.Second, 10*time.Millisecond, "application MCP must be live before opening the library client")

	// This opens while the application owner and its MCP registry are live.
	library, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(provider.URL, "library-key"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = library.Close() })

	result, err := library.Run(context.Background(), sdk.RunRequest{
		Prompt:      "Inspect rush_info and finish with the marker.",
		Mode:        sdk.RunModeJSON,
		Stdout:      io.Discard,
		HideSpinner: true,
	})
	require.NoError(t, err)
	require.Equal(t, marker, result.FinalText)

	first, second := provider.mainBodies(t)
	firstNames := r14ToolNamesFromBody(first)
	require.NotContains(t, firstNames, "mcp_"+serverName+"_"+toolName)
	require.NotContains(t, string(first), instructions)
	require.NotContains(t, string(first), "APPLICATION_ONLY_MCP_TOOL_SCHEMA")

	// The tool result is present in the next provider request. Its absence
	// proves rush_info did not report global runtime state or configured MCP
	// metadata from the application client.
	require.NotContains(t, string(second), serverName)
	require.NotContains(t, string(second), toolName)
	require.NotContains(t, string(second), instructions)
	require.NotContains(t, string(second), "[mcp]\\n")
	require.NotContains(t, string(second), "[mcp_configured]\\n")
}

func TestLibraryRunDoesNotWaitForPendingApplicationMCPInitialization(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const (
		serverName = "pending-application-mcp"
		marker     = "LIBRARY_RUN_DID_NOT_WAIT_FOR_MCP"
	)

	initializeStarted := make(chan struct{})
	releaseInitialize := make(chan struct{})
	var initializeOnce sync.Once
	mcpServer := modelmcp.NewServer(
		&modelmcp.Implementation{Name: serverName},
		&modelmcp.ServerOptions{},
	)
	delegate := modelmcp.NewStreamableHTTPHandler(
		func(*http.Request) *modelmcp.Server { return mcpServer }, nil,
	)
	mcpHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(`"method":"initialize"`)) ||
			bytes.Contains(body, []byte(`"method": "initialize"`)) {
			initializeOnce.Do(func() { close(initializeStarted) })
			select {
			case <-releaseInitialize:
			case <-r.Context().Done():
				return
			}
		}
		delegate.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		close(releaseInitialize)
		mcpHTTP.Close()
	})

	provider := newR17MCPIsolationProvider(t, marker, "pending-library-rush-info")
	applicationDir := t.TempDir()
	applicationConfig := map[string]any{
		"disable_default_providers": true,
		"providers": map[string]any{
			"probe": map[string]any{
				"id": "probe", "name": "probe", "type": "openai-compat",
				"base_url": provider.URL, "api_key": "probe", "discover_models": false,
				"models": []any{map[string]any{
					"id": "probe", "name": "probe", "context_window": 200000,
					"default_max_tokens": 1000,
				}},
			},
		},
		"models": map[string]any{
			"smart": map[string]string{"provider": "probe", "model": "probe"},
			"fast":  map[string]string{"provider": "probe", "model": "probe"},
		},
		"mcp": map[string]any{
			serverName: map[string]any{
				"type": "http", "url": mcpHTTP.URL, "enabled_in_cli": true,
			},
		},
	}
	configData, err := json.Marshal(applicationConfig)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(applicationDir, "rush.json"), configData, 0o600))

	application, err := sdk.Open(context.Background(), sdk.Options{
		WorkingDir: applicationDir,
		MCP:        sdk.MCPAll,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = application.Close() })

	select {
	case <-initializeStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("application MCP initialization did not reach the deterministic barrier")
	}

	library, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(provider.URL, "library-key"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = library.Close() })

	type runOutcome struct {
		result *sdk.RunResult
		err    error
	}
	runDone := make(chan runOutcome, 1)
	go func() {
		result, runErr := library.Run(context.Background(), sdk.RunRequest{
			Prompt:      "Inspect rush_info and finish with the marker.",
			Mode:        sdk.RunModeJSON,
			Stdout:      io.Discard,
			HideSpinner: true,
		})
		runDone <- runOutcome{result: result, err: runErr}
	}()

	select {
	case outcome := <-runDone:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.result)
		require.Equal(t, marker, outcome.result.FinalText)
	case <-time.After(10 * time.Second):
		t.Fatal("library Run waited for pending application MCP initialization")
	}
}

type r17MCPIsolationProvider struct {
	URL string

	mu     sync.Mutex
	bodies [][]byte
	turn   atomic.Int32
}

func newR17MCPIsolationProvider(t *testing.T, marker, callID string) *r17MCPIsolationProvider {
	t.Helper()
	provider := &r17MCPIsolationProvider{}
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte("Generate a concise title")) {
			sseChunks(t, w, []map[string]any{textChunk("probe", "title"), finishChunk("probe", "stop")})
			return
		}

		provider.mu.Lock()
		provider.bodies = append(provider.bodies, append([]byte(nil), body...))
		turn := provider.turn.Add(1)
		provider.mu.Unlock()
		if turn == 1 {
			sseChunks(t, w, []map[string]any{
				toolCallChunkNamed("probe", callID, "rush_info", map[string]any{}),
				finishChunk("probe", "tool_calls"),
			})
			return
		}
		sseChunks(t, w, []map[string]any{textChunk("probe", marker), finishChunk("probe", "stop")})
	}))
	provider.URL = providerServer.URL
	t.Cleanup(providerServer.Close)
	return provider
}

func (p *r17MCPIsolationProvider) mainBodies(t *testing.T) ([]byte, []byte) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	require.Len(t, p.bodies, 2)
	return p.bodies[0], p.bodies[1]
}
