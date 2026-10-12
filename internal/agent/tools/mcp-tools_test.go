package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestMCPToolPreservesErrorEnvelope(t *testing.T) {
	server := modelmcp.NewServer(&modelmcp.Implementation{Name: "envelope"}, nil)
	modelmcp.AddTool(server, &modelmcp.Tool{Name: "call"}, func(_ context.Context, _ *modelmcp.CallToolRequest, input struct {
		Kind string `json:"kind"`
	},
	) (*modelmcp.CallToolResult, any, error) {
		result := &modelmcp.CallToolResult{IsError: input.Kind != "success"}
		if input.Kind != "empty" {
			result.Content = append(result.Content, &modelmcp.TextContent{Text: "unchanged"})
		}
		switch input.Kind {
		case "image":
			result.Content = append(result.Content, &modelmcp.ImageContent{Data: []byte{255, 1}, MIMEType: "image/png"})
		case "audio":
			result.Content = append(result.Content, &modelmcp.AudioContent{Data: []byte{255, 2}, MIMEType: "audio/wav"})
		}
		return result, nil, nil
	})
	httpServer := httptest.NewServer(modelmcp.NewStreamableHTTPHandler(func(*http.Request) *modelmcp.Server { return server }, nil))
	defer httpServer.Close()
	owner, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	cfg := config.NewLibraryStore(&config.Config{MCP: config.MCPs{"envelope": {Type: config.MCPHttp, URL: httpServer.URL}}}, "")
	owner.Initialize(context.Background(), nil, cfg, false)
	require.NoError(t, owner.WaitForInit(context.Background()))
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session")
	ctx = context.WithValue(ctx, SupportsImagesContextKey, true)
	tool := &Tool{mcpName: "envelope", tool: &mcp.Tool{Name: "call"}, cfg: cfg, permissions: &mockPermissionService{}, owner: owner}
	for _, managed := range []bool{false, true} {
		cancelCtx, cancel := context.WithCancel(ctx)
		if managed {
			cancelCtx = WithBatchManagedCall(cancelCtx)
		}
		cancel()
		response, err := tool.Run(cancelCtx, fantasy.ToolCall{ID: "cancel", Name: tool.Name(), Input: `{}`})
		if managed {
			require.ErrorIs(t, err, context.Canceled)
		} else {
			require.NoError(t, err)
		}
		require.True(t, response.IsError)
		require.Contains(t, response.Content, "context canceled")
	}
	for _, kind := range []string{"text", "empty", "image", "audio", "success"} {
		t.Run(kind, func(t *testing.T) {
			input := `{"kind":"` + kind + `"}`
			result, err := owner.RunTool(ctx, cfg, "envelope", "call", input)
			require.NoError(t, err)
			require.Equal(t, kind != "success", result.IsError)
			response, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: tool.Name(), Input: input})
			require.NoError(t, err)
			require.Equal(t, result.IsError, response.IsError)
			encoded, err := json.Marshal(response)
			require.NoError(t, err)
			var decoded fantasy.ToolResponse
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			require.Equal(t, response, decoded)
			require.Equal(t, result.Content, response.Content)
			require.Equal(t, result.Data, response.Data)
			require.Equal(t, result.MediaType, response.MediaType)
		})
	}
}
