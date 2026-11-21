package childagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/clibridge"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
)

type PinnedInvoke func(context.Context, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error)

type CLIBridge struct {
	registry   *toolwrap.PolicyLeafRegistry
	invoke     PinnedInvoke
	advertised []cliprovider.BridgeTool
	builtin    map[string]string
}

func NewCLIBridge(ctx context.Context, registry *toolwrap.PolicyLeafRegistry, invoke PinnedInvoke, external []cliprovider.BridgeTool) (*CLIBridge, error) {
	if registry == nil || invoke == nil {
		return nil, fmt.Errorf("CLI bridge requires pinned registry and ingress")
	}
	definitions, err := cliprovider.BuiltinBridgeTools()
	if err != nil {
		return nil, err
	}
	bridge := &CLIBridge{registry: registry, invoke: invoke, builtin: make(map[string]string)}
	seenWire, seenCanonical := map[string]bool{}, map[string]bool{}
	for _, definition := range definitions {
		seenWire[definition.WireName], seenCanonical[definition.CanonicalName] = true, true
		if !toolwrap.RoleToolAllowed(ctx, definition.CanonicalName) {
			continue
		}
		if _, err := registry.ResolveForContext(ctx, definition.CanonicalName); err != nil {
			continue
		}
		bridge.advertised = append(bridge.advertised, definition)
		bridge.builtin[definition.CanonicalName] = definition.WireName
		seenWire[definition.WireName], seenCanonical[definition.CanonicalName] = true, true
	}
	for _, definition := range external {
		if !strings.Contains(definition.WireName, "__") || !strings.HasPrefix(definition.CanonicalName, "mcp_") || seenWire[definition.WireName] || seenCanonical[definition.CanonicalName] {
			return nil, fmt.Errorf("ambiguous CLI bridge mapping %q -> %q", definition.WireName, definition.CanonicalName)
		}
		if !toolwrap.RoleToolAllowed(ctx, definition.CanonicalName) {
			continue
		}
		if _, err := registry.ResolveForContext(ctx, definition.CanonicalName); err != nil {
			continue
		}
		definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
		bridge.advertised = append(bridge.advertised, definition)
		seenWire[definition.WireName], seenCanonical[definition.CanonicalName] = true, true
	}
	return bridge, nil
}

func (b *CLIBridge) Tools() []cliprovider.BridgeTool {
	result := append([]cliprovider.BridgeTool(nil), b.advertised...)
	for i := range result {
		result[i].InputSchema = append(json.RawMessage(nil), result[i].InputSchema...)
	}
	return result
}

func (b *CLIBridge) Call(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if err := toolwrap.GuardRoleToolCall(ctx, call.Name); err != nil {
		return fantasy.ToolResponse{}, err
	}
	accepted := false
	for _, tool := range b.advertised {
		if tool.CanonicalName == call.Name {
			accepted = true
			break
		}
	}
	if !accepted {
		return fantasy.ToolResponse{}, fmt.Errorf("CLI bridge target %q is not advertised", call.Name)
	}
	leaf, err := b.registry.ResolveForContext(ctx, call.Name)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	wire := b.builtin[call.Name]
	input, path, err := clibridge.TranslateCLIInput(wire, call.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	call.Input = input
	response, err := b.invoke(ctx, leaf, call)
	if err == nil && wire == "Read" && response.Type == "text" {
		response.Content = cliprovider.FilterBridgeRead(path, response.Content)
	}
	return response, err
}
