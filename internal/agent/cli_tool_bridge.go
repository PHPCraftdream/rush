package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

type pinnedInvoke func(context.Context, fantasy.AgentTool, fantasy.ToolCall) (fantasy.ToolResponse, error)

type cliToolBridge struct {
	registry   *policyLeafRegistry
	invoke     pinnedInvoke
	advertised []cliprovider.BridgeTool
	builtin    map[string]string
}

func newCLIToolBridge(registry *policyLeafRegistry, invoke pinnedInvoke, external []cliprovider.BridgeTool) (*cliToolBridge, error) {
	if registry == nil || invoke == nil {
		return nil, fmt.Errorf("CLI bridge requires pinned registry and ingress")
	}
	definitions, err := cliprovider.BuiltinBridgeTools()
	if err != nil {
		return nil, err
	}
	bridge := &cliToolBridge{registry: registry, invoke: invoke, builtin: make(map[string]string)}
	seenWire, seenCanonical := map[string]bool{}, map[string]bool{}
	for _, definition := range definitions {
		seenWire[definition.WireName], seenCanonical[definition.CanonicalName] = true, true
		if _, err := registry.resolve(definition.CanonicalName); err != nil {
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
		if _, err := registry.resolve(definition.CanonicalName); err != nil {
			continue
		}
		definition.InputSchema = append(json.RawMessage(nil), definition.InputSchema...)
		bridge.advertised = append(bridge.advertised, definition)
		seenWire[definition.WireName], seenCanonical[definition.CanonicalName] = true, true
	}
	return bridge, nil
}

func (b *cliToolBridge) Tools() []cliprovider.BridgeTool {
	result := append([]cliprovider.BridgeTool(nil), b.advertised...)
	for i := range result {
		result[i].InputSchema = append(json.RawMessage(nil), result[i].InputSchema...)
	}
	return result
}

func (b *cliToolBridge) Call(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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
	leaf, err := b.registry.resolve(call.Name)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	wire := b.builtin[call.Name]
	input, path, err := translateCLIInput(wire, call.Input)
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

func translateCLIInput(wire, input string) (string, string, error) {
	var params map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &params); err != nil {
		return "", "", err
	}
	if params == nil {
		return "", "", fmt.Errorf("CLI input must be an object")
	}
	path := ""
	switch wire {
	case "Read":
		var read struct {
			Path  string `json:"path"`
			Start int    `json:"start_line"`
			End   int    `json:"end_line"`
		}
		if err := json.Unmarshal([]byte(input), &read); err != nil {
			return "", "", err
		}
		if read.Start < 0 || read.End < 0 {
			return "", "", fmt.Errorf("Read range cannot be negative")
		}
		start := max(1, read.Start)
		if read.End != 0 && read.End < start {
			return "", "", fmt.Errorf("Read end precedes start")
		}
		limit := tools.DefaultReadLimit
		if read.End != 0 {
			limit = read.End - start + 1
		}
		raw, err := json.Marshal(tools.ViewParams{FilePath: read.Path, Offset: start - 1, Limit: limit})
		return string(raw), read.Path, err
	case "Write":
		params["file_path"] = params["path"]
		delete(params, "path")
	case "Grep":
		if glob, ok := params["glob"]; ok {
			params["include"] = glob
			delete(params, "glob")
		}
	case "Bash":
		for _, forbidden := range []string{"timeout", "run_in_background", "auto_background_after", "background"} {
			if _, ok := params[forbidden]; ok {
				return "", "", fmt.Errorf("CLI Bash does not accept %s", forbidden)
			}
		}
	}
	if wire == "" {
		return input, path, nil
	}
	raw, err := json.Marshal(params)
	return string(raw), path, err
}
