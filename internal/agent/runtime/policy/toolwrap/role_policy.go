package toolwrap

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools/hooks"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// ErrRoleToolDenied marks a dispatch rejected by a custom role's tool ceiling.
var ErrRoleToolDenied = errors.New("custom role tool policy denied the tool")

// PolicyToolBuild retains the registry belonging to one construction only.
// The model tools keep their legacy wrappers; the registry has no consumer.
type PolicyToolBuild struct {
	Tools      []fantasy.AgentTool
	Leaves     *PolicyLeafRegistry
	RegistryID string
}

// PolicyLeafRegistry is the shared registry type: batch ingress workers store
// and resolve pinned leaves through it.
type PolicyLeafRegistry struct {
	entries   map[string]policyLeafEntry
	contracts []*session.RoleContract
}

// policyLeafEntry records the original layers and the policy-only execution.
type policyLeafEntry struct {
	layers []string
	tool   fantasy.AgentTool
}

func NewPolicyToolBuild(tools []fantasy.AgentTool) PolicyToolBuild {
	return NewPolicyToolBuildForContext(context.Background(), tools)
}

func NewPolicyToolBuildForContext(ctx context.Context, tools []fantasy.AgentTool) PolicyToolBuild {
	registry := &PolicyLeafRegistry{entries: make(map[string]policyLeafEntry, len(tools))}
	activeContracts := CloneRoleToolContracts(roleToolContractsFrom(ctx))
	registry.contracts = mergeRoleToolContracts(activeContracts)
	filtered := make([]fantasy.AgentTool, 0, len(tools))
	for _, tool := range tools {
		capturedContracts := roleContractsFromTool(tool)
		if !RoleToolAllowed(ctx, tool.Info().Name) || !roleContractsAllowName(capturedContracts, tool.Info().Name) {
			continue
		}
		guardContracts := CloneRoleToolContracts(capturedContracts)
		for _, active := range activeContracts {
			if !hasRoleContract(guardContracts, active) {
				guardContracts = append(guardContracts, active.Clone())
			}
		}
		if len(guardContracts) > 0 {
			tool = NewRoleGuardTool(tool, guardContracts)
		}
		filtered = append(filtered, tool)
		leaf, layers := policyLeafExecution(tool)
		registry.entries[tool.Info().Name] = policyLeafEntry{layers: layers, tool: leaf}
	}
	return PolicyToolBuild{Tools: filtered, Leaves: registry}
}

// CloneLeaves copies the entry map inside one traversal, applying transform
// to each entry's tool, and deliberately drops the stored contracts, exactly
// as the batch registry store's original Register copy did.
func (r *PolicyLeafRegistry) CloneLeaves(transform func(name string, tool fantasy.AgentTool) fantasy.AgentTool) *PolicyLeafRegistry {
	if r == nil {
		return nil
	}
	clone := &PolicyLeafRegistry{entries: make(map[string]policyLeafEntry, len(r.entries))}
	for name, entry := range r.entries {
		if transform != nil {
			entry.tool = transform(name, entry.tool)
		}
		clone.entries[name] = entry
	}
	return clone
}

func (r *PolicyLeafRegistry) toolsetFingerprint() string {
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

func (r *PolicyLeafRegistry) HasToolset(fingerprint string) bool {
	return r != nil && fingerprint != "" && fingerprint == r.toolsetFingerprint()
}

func (r *PolicyLeafRegistry) Resolve(name string) (fantasy.AgentTool, error) {
	return r.ResolveForContext(context.Background(), name)
}

func (r *PolicyLeafRegistry) ResolveForContext(ctx context.Context, name string) (fantasy.AgentTool, error) {
	if r != nil {
		if !RoleToolAllowed(ctx, name) || !roleContractsAllowName(r.contracts, name) {
			return nil, guardRoleContractError(name)
		}
		if entry, ok := r.entries[name]; ok {
			return entry.tool, nil
		}
	}
	return nil, fmt.Errorf("policy leaf %q is unavailable in pinned toolset", name)
}

func (r *PolicyLeafRegistry) allRoleContracts() []*session.RoleContract {
	if r == nil {
		return nil
	}
	return CloneRoleToolContracts(r.contracts)
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

func RoleContractForToolBuild(ctx context.Context, tools []fantasy.AgentTool) *session.RoleContract {
	captured := contractFromContractGroups(RoleContractsForTools(tools))
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

// RoleContractsAllowName reports whether every contract allows the name.
func RoleContractsAllowName(contracts []*session.RoleContract, name string) bool {
	return roleContractsAllowName(contracts, name)
}

// policyLeafWrapped is the structural introspection seam for root-owned
// wrappers that stay outside this package: a PolicyLeafTool method hands the
// wrapped tool to role-contract introspection without wrapper-copying.
type policyLeafWrapped interface {
	PolicyLeafTool() fantasy.AgentTool
}

func roleContractsFromTool(tool fantasy.AgentTool) []*session.RoleContract {
	var contracts []*session.RoleContract
	for tool != nil {
		switch wrapped := tool.(type) {
		case roleGuardTool:
			contracts = append(contracts, CloneRoleToolContracts(wrapped.contracts)...)
			tool = wrapped.AgentTool
		case *roleGuardTool:
			contracts = append(contracts, CloneRoleToolContracts(wrapped.contracts)...)
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
		case *BatchIngressTool:
			tool = wrapped.AgentTool
		case *PrivateFetchIngressTool:
			tool = wrapped.AgentTool
		case policyLeafWrapped:
			tool = wrapped.PolicyLeafTool()
		default:
			return mergeRoleToolContracts(contracts)
		}
	}
	return mergeRoleToolContracts(contracts)
}

func RoleContractsForTools(tools []fantasy.AgentTool) []*session.RoleContract {
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
		copy.contracts = CloneRoleToolContracts(t.contracts)
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

// DecoratePolicyTools preserves hook-before-restricted execution and child
// hook omission. Permissions, scope and agentguard remain in the same tools.
func DecoratePolicyTools(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer, runner *hooks.Runner, child bool) []fantasy.AgentTool {
	return wrapToolsWithHooks(wrapToolsWithRestrictedRun(tools, authorizer), runner, child)
}

func DecorateSessionTools(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer) []fantasy.AgentTool {
	return wrapToolsWithErrorLogging(DecoratePolicyTools(tools, authorizer, nil, true))
}

type roleToolContractContextKey struct{}

func WithRoleToolContract(ctx context.Context, contract *session.RoleContract) context.Context {
	if contract == nil {
		return ctx
	}
	return context.WithValue(ctx, roleToolContractContextKey{}, contract.Clone())
}

func roleToolContractsFrom(ctx context.Context) []*session.RoleContract {
	if ctx == nil {
		return nil
	}
	var contracts []*session.RoleContract
	if opts := agentcall.CallOptionsFrom(ctx); opts != nil && opts.RoleContract != nil {
		contracts = append(contracts, opts.RoleContract)
	}
	if contract, _ := ctx.Value(roleToolContractContextKey{}).(*session.RoleContract); contract != nil {
		contracts = append(contracts, contract)
	}
	return contracts
}

func roleContractFromContext(ctx context.Context) *session.RoleContract {
	contracts := roleToolContractsFrom(ctx)
	if len(contracts) == 0 {
		return nil
	}
	return contracts[0]
}

func RoleContractFromContext(ctx context.Context) *session.RoleContract {
	return roleContractFromContext(ctx)
}

// roleContractAllowsName applies the role's own list and each inherited group
// as an intersection. Nil is unrestricted; an empty group denies every name.
func roleContractAllowsName(contract *session.RoleContract, name string) bool {
	if contract == nil {
		return true
	}
	if contract.Validate() != nil {
		return false
	}
	if contract.Tools != nil && !config.RoleToolsAllow(*contract.Tools, name) {
		return false
	}
	for _, inherited := range contract.Inherited {
		if !config.RoleToolsAllow(inherited, name) {
			return false
		}
	}
	return true
}

func RoleContractAllowsName(contract *session.RoleContract, name string) bool {
	return roleContractAllowsName(contract, name)
}

func RoleContractAllowsTool(contract *session.RoleContract, name string) bool {
	return roleContractAllowsName(contract, name)
}

// RoleToolAllowed checks every independently carried contract. Batch payload
// policy and the current CallOptions are conjunctive, never replacements.
func RoleToolAllowed(ctx context.Context, name string) bool {
	for _, contract := range roleToolContractsFrom(ctx) {
		if !roleContractAllowsName(contract, name) {
			return false
		}
	}
	return true
}

func RoleToolsConstrained(ctx context.Context) bool {
	for _, contract := range roleToolContractsFrom(ctx) {
		if contract == nil || contract.Validate() != nil || contract.Tools != nil || len(contract.Inherited) > 0 {
			return true
		}
	}
	return false
}

func GuardRoleToolCall(ctx context.Context, name string) error {
	if RoleToolAllowed(ctx, name) {
		return nil
	}
	return fmt.Errorf("%w: %q is outside the current role's allowed tools", ErrRoleToolDenied, name)
}

func FilterRoleToolNames(ctx context.Context, names []string) []string {
	if len(roleToolContractsFrom(ctx)) == 0 {
		return names
	}
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if RoleToolAllowed(ctx, name) {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

func FilterRoleContractTools(ctx context.Context, tools []fantasy.AgentTool) []fantasy.AgentTool {
	currentContracts := roleToolContractsFrom(ctx)
	filtered := make([]fantasy.AgentTool, 0, len(tools))
	for _, tool := range tools {
		capturedContracts := roleContractsFromTool(tool)
		if !RoleToolAllowed(ctx, tool.Info().Name) || !roleContractsAllowName(capturedContracts, tool.Info().Name) {
			continue
		}
		guardContracts := CloneRoleToolContracts(capturedContracts)
		for _, active := range currentContracts {
			if !hasRoleContract(guardContracts, active) {
				guardContracts = append(guardContracts, active.Clone())
			}
		}
		if len(guardContracts) > 0 {
			filtered = append(filtered, NewRoleGuardTool(tool, guardContracts))
		} else {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

// policyLeaf is the read-only boundary view of one registry entry. Unexported:
// callers infer the type from the leaf method and only touch its fields.
type policyLeaf struct {
	Tool   fantasy.AgentTool
	Layers []string
}

func (r *PolicyLeafRegistry) leaf(name string) (policyLeaf, bool) {
	if r == nil {
		return policyLeaf{}, false
	}
	entry, ok := r.entries[name]
	return policyLeaf{Tool: entry.tool, Layers: entry.layers}, ok
}

func (r *PolicyLeafRegistry) leafNames() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.entries))
	for name := range r.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func hasRoleContract(contracts []*session.RoleContract, target *session.RoleContract) bool {
	for _, existing := range contracts {
		if existing.Equal(target) {
			return true
		}
	}
	return false
}

func HasRoleContract(contracts []*session.RoleContract, target *session.RoleContract) bool {
	return hasRoleContract(contracts, target)
}

func CloneRoleToolContracts(contracts []*session.RoleContract) []*session.RoleContract {
	clones := make([]*session.RoleContract, 0, len(contracts))
	for _, contract := range contracts {
		if contract == nil {
			continue
		}
		clones = append(clones, contract.Clone())
	}
	return clones
}

// roleGuardTool retains the build-time cap and also checks the invocation's
// live cap, protecting stale pinned leaves and tools refreshed on shared agents.
type roleGuardTool struct {
	fantasy.AgentTool
	contracts []*session.RoleContract
}

func NewRoleGuardTool(agentTool fantasy.AgentTool, contracts []*session.RoleContract) roleGuardTool {
	return roleGuardTool{AgentTool: agentTool, contracts: contracts}
}

func (t roleGuardTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	canonical := t.AgentTool.Info().Name
	if call.Name != "" && call.Name != canonical {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("%v: forged tool name %q does not match %q", ErrRoleToolDenied, call.Name, canonical)), nil
	}
	for _, contract := range t.contracts {
		if !roleContractAllowsName(contract, canonical) {
			return fantasy.NewTextErrorResponse(guardRoleContractError(canonical).Error()), nil
		}
	}
	if err := GuardRoleToolCall(ctx, canonical); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return t.AgentTool.Run(ctx, call)
}

func guardRoleContractError(name string) error {
	return fmt.Errorf("%w: %q is outside the current role's allowed tools", ErrRoleToolDenied, name)
}

func GuardRoleContractError(name string) error {
	return guardRoleContractError(name)
}

func (t roleGuardTool) ProviderOptions() fantasy.ProviderOptions {
	return t.AgentTool.ProviderOptions()
}

func (t roleGuardTool) SetProviderOptions(options fantasy.ProviderOptions) {
	t.AgentTool.SetProviderOptions(options)
}

// RoleToolContractsFrom exposes the context contract extraction to the root
// bridge; the conjunctive extraction stays defined here only.
func RoleToolContractsFrom(ctx context.Context) []*session.RoleContract {
	return roleToolContractsFrom(ctx)
}

// pruneRoleContractTools keeps the legacy prune name over the filter body;
// retained because tests exercise the prune path against the shared manifest.
func pruneRoleContractTools(ctx context.Context, tools []fantasy.AgentTool) []fantasy.AgentTool {
	return FilterRoleContractTools(ctx, tools)
}
