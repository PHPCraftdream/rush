package agent

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/hooks"
	"github.com/PHPCraftdream/rush/internal/permission"
)

// policyToolBuild retains the registry belonging to one construction only.
// The model tools keep their legacy wrappers; the registry has no consumer.
type policyToolBuild struct {
	tools      []fantasy.AgentTool
	leaves     *policyLeafRegistry
	registryID string
}

// policyLeafEntry records the original layers and the policy-only execution.
type policyLeafEntry struct {
	layers []string
	tool   fantasy.AgentTool
}

type policyLeafRegistry struct {
	entries map[string]policyLeafEntry
}

func newPolicyToolBuild(tools []fantasy.AgentTool) policyToolBuild {
	registry := &policyLeafRegistry{entries: make(map[string]policyLeafEntry, len(tools))}
	for _, tool := range tools {
		leaf, layers := policyLeafExecution(tool)
		registry.entries[tool.Info().Name] = policyLeafEntry{layers: layers, tool: leaf}
	}
	return policyToolBuild{tools: tools, leaves: registry}
}

func (r *policyLeafRegistry) hasToolset(fingerprint string) bool {
	if r == nil || fingerprint == "" {
		return false
	}
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return fingerprint == strings.Join(names, ",")
}

func (r *policyLeafRegistry) resolve(name string) (fantasy.AgentTool, error) {
	if r != nil {
		if entry, ok := r.entries[name]; ok {
			return entry.tool, nil
		}
	}
	return nil, fmt.Errorf("policy leaf %q is unavailable in pinned toolset", name)
}

// policyLeafExecution removes only execution-lifetime wrappers. Policy
// wrappers are copied, retaining their runner/authorizer and underlying tool.
func policyLeafExecution(tool fantasy.AgentTool) (fantasy.AgentTool, []string) {
	var inner fantasy.AgentTool
	var layer string
	switch t := tool.(type) {
	case *asyncTool:
		inner, layer = t.inner, "async"
	case *stallDetacherTool:
		inner, layer = t.inner, "stall"
	case *hookedTool:
		leaf, layers := policyLeafExecution(t.inner)
		copy := *t
		copy.inner = leaf
		return &copy, append([]string{"hooks"}, layers...)
	case *restrictedTool:
		leaf, layers := policyLeafExecution(t.AgentTool)
		copy := *t
		copy.AgentTool = leaf
		return &copy, append([]string{"restricted"}, layers...)
	case restrictedTool:
		leaf, layers := policyLeafExecution(t.AgentTool)
		t.AgentTool = leaf
		return t, append([]string{"restricted"}, layers...)
	case *loggedTool:
		leaf, layers := policyLeafExecution(t.inner)
		copy := *t
		copy.inner = leaf
		return &copy, append([]string{"logging"}, layers...)
	default:
		return tool, []string{"tool"}
	}
	leaf, layers := policyLeafExecution(inner)
	return leaf, append([]string{layer}, layers...)
}

// decoratePolicyTools preserves hook-before-restriction execution and child
// hook omission. Permissions, scope and agentguard remain in the same tools.
func decoratePolicyTools(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer, runner *hooks.Runner, child bool) []fantasy.AgentTool {
	return wrapToolsWithHooks(wrapToolsWithRestrictedRun(tools, authorizer), runner, child)
}

func decorateSessionTools(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer) []fantasy.AgentTool {
	return wrapToolsWithErrorLogging(decoratePolicyTools(tools, authorizer, nil, true))
}
