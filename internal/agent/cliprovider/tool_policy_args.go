package cliprovider

import (
	"fmt"
	"slices"
	"strings"
)

// applyRestrictedToolArgs keeps Claude's native tool surface empty and
// advertises only role-approved Rush bridge tools. Claude's --tools flag is
// significant under bypass mode; --allowedTools alone is not a capability
// boundary there.
func policyBridgeTools(policy ToolPolicy, bridge ToolBridge, proxy ExternalMCPProxy) ([]BridgeTool, error) {
	if bridge != nil {
		var allowed []BridgeTool
		for _, tool := range bridge.Tools() {
			if policyHasMCPTool(policy, tool.CanonicalName) {
				allowed = append(allowed, tool)
			}
		}
		return allowed, nil
	}
	if policy.Constrained && proxy != nil && len(proxy.ListTools()) > 0 {
		return nil, fmt.Errorf("constrained external MCP tools require an invocation-pinned bridge")
	}
	var allowed []BridgeTool
	builtins, err := BuiltinBridgeTools()
	if err != nil {
		return nil, err
	}
	for _, tool := range builtins {
		if policyHasMCPTool(policy, tool.CanonicalName) {
			allowed = append(allowed, tool)
		}
	}
	if proxy != nil {
		for _, external := range proxy.ListTools() {
			canonical := "mcp_" + external.ServerName + "_" + external.Name
			if policyHasMCPTool(policy, canonical) {
				allowed = append(allowed, BridgeTool{WireName: external.ServerName + "__" + external.Name, CanonicalName: canonical})
			}
		}
	}
	return allowed, nil
}

func applyRestrictedToolArgs(args []string, spec CLISpec, bridge ToolBridge, proxy ExternalMCPProxy, policy ToolPolicy) ([]string, error) {
	if !spec.SupportsRestrictedTools {
		return nil, fmt.Errorf("cliprovider: %s cannot enforce this invocation's native-tool capability ceiling", spec.ModelID)
	}
	tools, err := policyBridgeTools(policy, bridge, proxy)
	if err != nil {
		return nil, err
	}
	switch spec.Binary {
	case "claude":
		allowed := make([]string, 0, len(tools))
		for _, tool := range tools {
			allowed = append(allowed, "mcp__rush__"+tool.WireName)
		}
		args = removeCLIFlags(args, "--tools", "--allowedTools", "--allowed-tools", "--disallowedTools")
		return append(args, "--tools", "", "--allowedTools", strings.Join(allowed, ","), "--disallowedTools", "TodoWrite"), nil
	case "qwen":
		// Native qwen commands, including autonomous delegation, are absent
		// from this explicit MCP-only allowlist. The integration block adds
		// the exact allowed Rush MCP names below.
		return args, nil
	default:
		return nil, fmt.Errorf("cliprovider: %s has no supported native-tool restriction interface", spec.ModelID)
	}
}

func removeCLIFlags(args []string, flags ...string) []string {
	blocked := make(map[string]struct{}, len(flags))
	for _, flag := range flags {
		blocked[flag] = struct{}{}
	}
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if _, ok := blocked[args[i]]; ok {
			i++ // all supported policy flags in BuildArgs take one value
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func cliPolicyAllows(policy ToolPolicy, name string) bool {
	return !policy.Constrained || slices.Contains(policy.Allowed, name)
}
