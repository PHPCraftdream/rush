package cliprovider

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

type bridgeSchemaSessions struct{ session.Service }

func TestToolBridgeExactLegacySchemas(t *testing.T) {
	definitions, err := BuiltinBridgeTools()
	require.NoError(t, err)
	srv, err := newRushMCPServer(WithToolBridge(t.Context(), nil), nil, bridgeSchemaSessions{}, "session", t.TempDir(), "test-token", nil)
	require.NoError(t, err)
	t.Cleanup(srv.stop)
	client := connectMCPTestClient(t, srv)
	listed, err := client.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, len(definitions))
	byName := map[string]BridgeTool{}
	for _, definition := range definitions {
		byName[definition.WireName] = definition
	}
	for _, tool := range listed.Tools {
		definition, ok := byName[tool.Name]
		require.True(t, ok)
		require.Equal(t, definition.Description, tool.Description, "Legacy wire description unchanged")
		raw, err := json.Marshal(tool.InputSchema)
		require.NoError(t, err)
		require.JSONEq(t, string(definition.InputSchema), string(raw), "Legacy full wire schema unchanged")
	}
}

func TestToolBridgeTrustedContextValues(t *testing.T) {
	request := context.WithValue(t.Context(), bridgeTrustedKey{}, "untrusted")
	trusted := context.WithValue(t.Context(), bridgeTrustedKey{}, "trusted")
	require.Equal(t, "trusted", (bridgeRequestContext{Context: request, invocation: trusted}).Value(bridgeTrustedKey{}), "Invocation trust must not use HTTP/global state")
}

func TestToolBridgeMediaEndToEnd(t *testing.T) {
	for _, response := range []fantasy.ToolResponse{
		fantasy.NewImageResponse([]byte{1, 2}, "image/png"),
		fantasy.NewMediaResponse([]byte{3, 4}, "audio/wav"),
		fantasy.NewMediaResponse([]byte{5}, "image/jpeg"),
		fantasy.NewTextErrorResponse("denial"),
	} {
		t.Run(response.Type+response.MediaType, func(t *testing.T) {
			response.IsError = true
			bridge := testToolBridge{tools: []BridgeTool{bridgeTestTool()}, call: func(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) { return response, nil }}
			client := connectMCPTestClient(t, newBridgeTestServer(t, t.Context(), bridge))
			result := callMCPTool(t, client, "Bash", map[string]any{"command": "media"})
			expected, err := bridgeMCPResult(response)
			require.NoError(t, err)
			require.Equal(t, expected, result, "MCP transport preserves media bytes/type and IsError")
		})
	}
}
