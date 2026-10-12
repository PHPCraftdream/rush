package cliprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// BridgeTool describes a wire tool and its pinned canonical target. InputSchema
// is the complete MCP input schema, not just the properties map.
type BridgeTool struct {
	WireName      string
	CanonicalName string
	Description   string
	InputSchema   json.RawMessage
}

// ToolBridge belongs to one enclosing invocation. Call owns policy and ingress;
// the provider never adds permission checks or dispatches a legacy substitute.
type ToolBridge interface {
	Tools() []BridgeTool
	Call(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error)
}

type toolBridgeKey struct{}

// WithToolBridge binds an optional bridge to a trusted invocation context.
// Nothing is read from HTTP headers or arguments to establish this binding.
// A nil bridge preserves the legacy path.
func WithToolBridge(ctx context.Context, bridge ToolBridge) context.Context {
	return context.WithValue(ctx, toolBridgeKey{}, bridge)
}

func invocationToolBridge(ctx context.Context) ToolBridge {
	bridge, _ := ctx.Value(toolBridgeKey{}).(ToolBridge)
	return bridge
}

// bridgeRequestContext keeps request cancellation but only trusted invocation
// values. In particular HTTP middleware cannot supply owner or policy identity.
type bridgeRequestContext struct {
	context.Context
	invocation context.Context
}

func (c bridgeRequestContext) Value(key any) any { return c.invocation.Value(key) }

func registerBridgeTools(invocation context.Context, srv *mcp.Server, bridge ToolBridge, toolCh chan mcpToolEvent) error {
	tools := bridge.Tools()
	seen := make(map[string]bool, len(tools))
	// Validate the whole advertisement before registering any callbacks.
	for _, tool := range tools {
		if tool.WireName == "" || tool.CanonicalName == "" || seen[tool.WireName] {
			return fmt.Errorf("cliprovider: invalid or duplicate bridge tool %q", tool.WireName)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema["type"] != "object" {
			return fmt.Errorf("cliprovider: bridge tool %q requires a full object schema", tool.WireName)
		}
		seen[tool.WireName] = true
	}
	for _, tool := range tools {
		schema := append(json.RawMessage(nil), tool.InputSchema...)
		srv.AddTool(&mcp.Tool{Name: tool.WireName, Description: tool.Description, InputSchema: schema},
			func(request context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				ctx, cancel := context.WithCancel(bridgeRequestContext{Context: request, invocation: invocation})
				stop := context.AfterFunc(invocation, cancel)
				defer stop()
				defer cancel()
				if err := invocation.Err(); err != nil {
					return nil, err
				}
				id := uuid.New().String()
				input := string(req.Params.Arguments)
				emitToolStart(toolCh, id, tool.WireName, input)
				defer emitToolEnd(toolCh, id)
				response, err := bridge.Call(ctx, fantasy.ToolCall{ID: id, Name: tool.CanonicalName, Input: input})
				if err != nil {
					return nil, err
				}
				return bridgeMCPResult(response)
			})
	}
	return nil
}

func bridgeMCPResult(response fantasy.ToolResponse) (*mcp.CallToolResult, error) {
	result := &mcp.CallToolResult{IsError: response.IsError}
	switch response.Type {
	case "text":
		result.Content = []mcp.Content{&mcp.TextContent{Text: response.Content}}
	case "image", "media":
		if strings.HasPrefix(response.MediaType, "image/") {
			result.Content = []mcp.Content{&mcp.ImageContent{Data: append([]byte(nil), response.Data...), MIMEType: response.MediaType}}
		} else if response.Type == "media" && strings.HasPrefix(response.MediaType, "audio/") {
			result.Content = []mcp.Content{&mcp.AudioContent{Data: append([]byte(nil), response.Data...), MIMEType: response.MediaType}}
		} else {
			return nil, fmt.Errorf("cliprovider: unsupported bridge media %q (%s)", response.MediaType, response.Type)
		}
		if response.Content != "" {
			result.Content = append([]mcp.Content{&mcp.TextContent{Text: response.Content}}, result.Content...)
		}
	default:
		return nil, fmt.Errorf("cliprovider: unsupported bridge response type %q", response.Type)
	}
	return result, nil
}
