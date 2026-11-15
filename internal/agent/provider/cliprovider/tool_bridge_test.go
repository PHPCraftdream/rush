package cliprovider

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type testToolBridge struct {
	tools []BridgeTool
	call  func(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error)
}

func (b testToolBridge) Tools() []BridgeTool { return b.tools }
func (b testToolBridge) Call(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return b.call(ctx, call)
}

func bridgeTestTool() BridgeTool {
	return BridgeTool{WireName: "Bash", CanonicalName: "bash", Description: "Pinned Bash", InputSchema: json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"],"additionalProperties":false}`)}
}

// Embedded nil services panic if a legacy execution or permission path runs.
type bridgeLegacyProxySpy struct{}

func (bridgeLegacyProxySpy) ListTools() []ExternalMCPTool { panic("legacy proxy list") }
func (bridgeLegacyProxySpy) CallTool(context.Context, string, string, string) (string, error) {
	panic("legacy proxy call")
}

func newBridgeTestServer(t *testing.T, ctx context.Context, bridge ToolBridge) *rushMCPServer {
	t.Helper()
	srv, err := newRushMCPServer(WithToolBridge(ctx, bridge), nil, nil, "", t.TempDir(), "test-token", bridgeLegacyProxySpy{})
	require.NoError(t, err)
	t.Cleanup(srv.stop)
	return srv
}

func TestToolBridgeExclusiveIdentityAndSchema(t *testing.T) {
	var got fantasy.ToolCall
	bridge := testToolBridge{tools: []BridgeTool{bridgeTestTool()}, call: func(_ context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
		got = call
		return fantasy.NewTextErrorResponse("pinned denial"), nil
	}}
	perms := &recordingMCPPermissionService{deny: true}
	srv, err := newRushMCPServer(WithToolBridge(t.Context(), bridge), perms, nil, "", t.TempDir(), "test-token", bridgeLegacyProxySpy{})
	require.NoError(t, err)
	t.Cleanup(srv.stop)
	client := connectMCPTestClient(t, srv)
	listed, err := client.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 1, "No legacy substitute advertised")
	require.Equal(t, "Bash", listed.Tools[0].Name)
	actual, err := json.Marshal(listed.Tools[0].InputSchema)
	require.NoError(t, err)
	require.JSONEq(t, string(bridgeTestTool().InputSchema), string(actual))
	result := callMCPTool(t, client, "Bash", map[string]any{"command": "claude"})
	require.True(t, result.IsError, "Pinned IsError must survive")
	require.Equal(t, "pinned denial", result.Content[0].(*mcp.TextContent).Text)
	require.Empty(t, perms.Requests(), "Configured bridge must not double-check legacy permission")
	require.Equal(t, "bash", got.Name, "Pinned canonical name, not raw wire name")
	_, err = uuid.Parse(got.ID)
	require.NoError(t, err)
	start, end := <-srv.toolCh, <-srv.toolCh
	require.Equal(t, got.ID, start.id, "Call and event identity must be unchanged")
	require.Equal(t, got.ID, end.id)
	require.Equal(t, "Bash", start.name)
	require.JSONEq(t, `{"command":"claude"}`, got.Input)
	missing, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "Read", Arguments: map[string]any{"path": "missing"}})
	require.True(t, err != nil || missing.IsError, "Missing pinned target must fail closed")
}

func TestToolBridgeTransportErrorNeverFallsBack(t *testing.T) {
	calls := 0
	bridge := testToolBridge{tools: []BridgeTool{bridgeTestTool()}, call: func(_ context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		calls++
		return fantasy.ToolResponse{}, errors.New("pinned transport failure")
	}}
	client := connectMCPTestClient(t, newBridgeTestServer(t, t.Context(), bridge))
	_, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: "Bash", Arguments: map[string]any{"command": "echo forbidden fallback"}})
	require.ErrorContains(t, err, "pinned transport failure", "Transport failure must not become a tool error or legacy dispatch")
	require.Equal(t, 1, calls)
}

func TestToolBridgeEmptyAndInvalidFailClosed(t *testing.T) {
	srv := newBridgeTestServer(t, t.Context(), testToolBridge{})
	client := connectMCPTestClient(t, srv)
	listed, err := client.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Empty(t, listed.Tools, "Empty pinned generation must not substitute defaults")
	for _, tools := range [][]BridgeTool{{{WireName: "Bash"}}, {bridgeTestTool(), bridgeTestTool()}, {{WireName: "Bash", CanonicalName: "bash", InputSchema: json.RawMessage(`{"properties":{}}`)}}} {
		_, err := newRushMCPServer(WithToolBridge(t.Context(), testToolBridge{tools: tools}), nil, nil, "", "", "test-token", bridgeLegacyProxySpy{})
		require.Error(t, err)
	}
}

func TestToolBridgeMedia(t *testing.T) {
	for _, test := range []struct {
		name     string
		response fantasy.ToolResponse
		want     mcp.Content
	}{
		{"text", fantasy.NewTextErrorResponse("denied"), &mcp.TextContent{Text: "denied"}},
		{"image", fantasy.NewImageResponse([]byte{1, 2}, "image/png"), &mcp.ImageContent{Data: []byte{1, 2}, MIMEType: "image/png"}},
		{"audio", fantasy.NewMediaResponse([]byte{3, 4}, "audio/wav"), &mcp.AudioContent{Data: []byte{3, 4}, MIMEType: "audio/wav"}},
		{"media image", fantasy.NewMediaResponse([]byte{5}, "image/jpeg"), &mcp.ImageContent{Data: []byte{5}, MIMEType: "image/jpeg"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.response.IsError = true
			result, err := bridgeMCPResult(test.response)
			require.NoError(t, err)
			require.True(t, result.IsError, "Media IsError must survive")
			require.Equal(t, []mcp.Content{test.want}, result.Content)
			if len(test.response.Data) > 0 {
				test.response.Data[0] = 99
				switch content := result.Content[0].(type) {
				case *mcp.ImageContent:
					require.NotEqual(t, test.response.Data, content.Data)
				case *mcp.AudioContent:
					require.NotEqual(t, test.response.Data, content.Data)
				}
			}
		})
	}
	for _, response := range []fantasy.ToolResponse{fantasy.NewMediaResponse([]byte{1}, "video/mp4"), fantasy.NewImageResponse(nil, "audio/wav"), {Type: "unknown"}} {
		result, err := bridgeMCPResult(response)
		require.ErrorContains(t, err, "unsupported bridge", "Unsupported media must not be silently dropped")
		require.Nil(t, result)
	}
}

type bridgeTrustedKey struct{}

func TestToolBridgeConcurrentInvocationBinding(t *testing.T) {
	var wg sync.WaitGroup
	for _, generation := range []string{"owner-a/generation-1", "owner-b/generation-2"} {
		ctx := context.WithValue(t.Context(), bridgeTrustedKey{}, generation)
		bridge := testToolBridge{tools: []BridgeTool{bridgeTestTool()}, call: func(ctx context.Context, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			require.Equal(t, generation, ctx.Value(bridgeTrustedKey{}), "Invocation generation must not come from global or HTTP state")
			return fantasy.NewTextResponse(generation), nil
		}}
		client := connectMCPTestClient(t, newBridgeTestServer(t, ctx, bridge))
		wg.Go(func() {
			result := callMCPTool(t, client, "Bash", map[string]any{"command": "test"})
			require.Equal(t, generation, result.Content[0].(*mcp.TextContent).Text)
		})
	}
	wg.Wait()
	request := context.WithValue(t.Context(), bridgeTrustedKey{}, "untrusted")
	trusted := context.WithValue(t.Context(), bridgeTrustedKey{}, "trusted")
	require.Equal(t, "trusted", (bridgeRequestContext{Context: request, invocation: trusted}).Value(bridgeTrustedKey{}))
}

func TestToolBridgeNilPreservesLegacy(t *testing.T) {
	ctx := WithToolBridge(t.Context(), nil)
	require.Nil(t, invocationToolBridge(ctx))
	dir := t.TempDir()
	perms := newMCPTestPermissionService(t, dir)
	perms.AutoApproveSession("owner")
	srv, err := newRushMCPServer(ctx, perms, nil, "owner", dir, "test-token", testExternalMCPProxy{})
	require.NoError(t, err)
	t.Cleanup(srv.stop)
	client := connectMCPTestClient(t, srv)
	listed, err := client.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, listed.Tools, 6)
	result := callMCPTool(t, client, "external__ping", map[string]any{})
	require.False(t, result.IsError)
	require.Equal(t, "external result", result.Content[0].(*mcp.TextContent).Text)
}
