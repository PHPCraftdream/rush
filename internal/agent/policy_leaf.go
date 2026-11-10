package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/hooks"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
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
	entries   map[string]policyLeafEntry
	contracts []*session.RoleContract
}

func newPolicyToolBuild(tools []fantasy.AgentTool) policyToolBuild {
	return newPolicyToolBuildForContext(context.Background(), tools)
}

func newPolicyToolBuildForContext(ctx context.Context, tools []fantasy.AgentTool) policyToolBuild {
	registry := &policyLeafRegistry{entries: make(map[string]policyLeafEntry, len(tools))}
	activeContracts := cloneRoleToolContracts(roleToolContractsFrom(ctx))
	registry.contracts = mergeRoleToolContracts(activeContracts)
	filtered := make([]fantasy.AgentTool, 0, len(tools))
	for _, tool := range tools {
		capturedContracts := roleContractsFromTool(tool)
		if !roleToolAllowed(ctx, tool.Info().Name) || !roleContractsAllowName(capturedContracts, tool.Info().Name) {
			continue
		}
		guardContracts := cloneRoleToolContracts(capturedContracts)
		for _, active := range activeContracts {
			if !hasRoleContract(guardContracts, active) {
				guardContracts = append(guardContracts, active.Clone())
			}
		}
		if len(guardContracts) > 0 {
			tool = roleGuardTool{AgentTool: tool, contracts: guardContracts}
		}
		filtered = append(filtered, tool)
		leaf, layers := policyLeafExecution(tool)
		registry.entries[tool.Info().Name] = policyLeafEntry{layers: layers, tool: leaf}
	}
	return policyToolBuild{tools: filtered, leaves: registry}
}

func (r *policyLeafRegistry) toolsetFingerprint() string {
	if r == nil {
		return ""
	}
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}

func (r *policyLeafRegistry) hasToolset(fingerprint string) bool {
	return r != nil && fingerprint != "" && fingerprint == r.toolsetFingerprint()
}

func (r *policyLeafRegistry) resolve(name string) (fantasy.AgentTool, error) {
	return r.resolveForContext(context.Background(), name)
}

func (r *policyLeafRegistry) resolveForContext(ctx context.Context, name string) (fantasy.AgentTool, error) {
	if r != nil {
		if !roleToolAllowed(ctx, name) || !roleContractsAllowName(r.contracts, name) {
			return nil, guardRoleContractError(name)
		}
		if entry, ok := r.entries[name]; ok {
			return entry.tool, nil
		}
	}
	return nil, fmt.Errorf("policy leaf %q is unavailable in pinned toolset", name)
}

func (r *policyLeafRegistry) allRoleContracts() []*session.RoleContract {
	if r == nil {
		return nil
	}
	return cloneRoleToolContracts(r.contracts)
}

func contractFromContractGroups(contracts []*session.RoleContract) *session.RoleContract {
	merged := mergeRoleToolContracts(contracts)
	if len(merged) == 0 {
		return nil
	}
	return merged[0]
}

func mergeRoleToolContracts(contracts []*session.RoleContract) []*session.RoleContract {
	groups := make([][]string, 0)
	for _, contract := range contracts {
		if contract == nil || contract.Validate() != nil {
			// A malformed captured cap must poison the merged policy rather
			// than disappear and widen the registry.
			return []*session.RoleContract{{Version: 0}}
		}
		if contract.Tools != nil {
			groups = append(groups, append([]string{}, (*contract.Tools)...))
		}
		for _, inherited := range contract.Inherited {
			groups = append(groups, append([]string{}, inherited...))
		}
	}
	if len(groups) == 0 {
		return nil
	}
	merged := &session.RoleContract{Version: session.RoleContractVersion, Inherited: groups}
	if merged.Validate() != nil {
		return []*session.RoleContract{{Version: 0}}
	}
	return []*session.RoleContract{merged}
}

func roleContractForToolBuild(ctx context.Context, tools []fantasy.AgentTool) *session.RoleContract {
	captured := contractFromContractGroups(roleContractsForTools(tools))
	current := roleContractFromContext(ctx)
	if current == nil {
		return captured
	}
	if captured == nil {
		return current.Clone()
	}
	merged := current.Clone()
	if captured.Tools != nil {
		merged.Inherited = append(merged.Inherited, append([]string{}, (*captured.Tools)...))
	}
	for _, group := range captured.Inherited {
		merged.Inherited = append(merged.Inherited, append([]string{}, group...))
	}
	return merged
}

func roleContractsAllowName(contracts []*session.RoleContract, name string) bool {
	for _, contract := range contracts {
		if !roleContractAllowsName(contract, name) {
			return false
		}
	}
	return true
}

func roleContractsFromTool(tool fantasy.AgentTool) []*session.RoleContract {
	var contracts []*session.RoleContract
	for tool != nil {
		switch wrapped := tool.(type) {
		case roleGuardTool:
			contracts = append(contracts, cloneRoleToolContracts(wrapped.contracts)...)
			tool = wrapped.AgentTool
		case *roleGuardTool:
			contracts = append(contracts, cloneRoleToolContracts(wrapped.contracts)...)
			tool = wrapped.AgentTool
		case *hookedTool:
			tool = wrapped.inner
		case restrictedTool:
			tool = wrapped.AgentTool
		case *restrictedTool:
			tool = wrapped.AgentTool
		case *loggedTool:
			tool = wrapped.inner
		case *fatalToolTool:
			tool = wrapped.inner
		case *providerOptionsTool:
			tool = wrapped.inner
		case *progressGuardTool:
			tool = wrapped.inner
		case *batchIngressTool:
			tool = wrapped.AgentTool
		case *privateFetchIngressTool:
			tool = wrapped.AgentTool
		case *batchCredentialLeaf:
			tool = wrapped.AgentTool
		case *pctCapturedAgentTool:
			tool = wrapped.AgentTool
		default:
			return mergeRoleToolContracts(contracts)
		}
	}
	return mergeRoleToolContracts(contracts)
}

func roleContractsForTools(tools []fantasy.AgentTool) []*session.RoleContract {
	var contracts []*session.RoleContract
	for _, tool := range tools {
		contracts = append(contracts, roleContractsFromTool(tool)...)
	}
	return mergeRoleToolContracts(contracts)
}

// wrappers are copied, retaining their runner/authorizer and underlying tool.
func policyLeafExecution(tool fantasy.AgentTool) (fantasy.AgentTool, []string) {
	switch t := tool.(type) {
	case roleGuardTool:
		leaf, layers := policyLeafExecution(t.AgentTool)
		t.AgentTool = leaf
		return t, append([]string{"role"}, layers...)
	case *roleGuardTool:
		leaf, layers := policyLeafExecution(t.AgentTool)
		copy := *t
		copy.AgentTool = leaf
		copy.contracts = cloneRoleToolContracts(t.contracts)
		return &copy, append([]string{"role"}, layers...)
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
}

// decoratePolicyTools preserves hook-before-restriction execution and child
// hook omission. Permissions, scope and agentguard remain in the same tools.
func decoratePolicyTools(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer, runner *hooks.Runner, child bool) []fantasy.AgentTool {
	return wrapToolsWithHooks(wrapToolsWithRestrictedRun(tools, authorizer), runner, child)
}

func decorateSessionTools(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer) []fantasy.AgentTool {
	return wrapToolsWithErrorLogging(decoratePolicyTools(tools, authorizer, nil, true))
}
