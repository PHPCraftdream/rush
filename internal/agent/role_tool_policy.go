package agent

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
)

// ErrRoleToolDenied marks a dispatch rejected by a custom role's tool ceiling.
var ErrRoleToolDenied = errors.New("custom role tool policy denied the tool")

type roleToolContractContextKey struct{}

func withRoleToolContract(ctx context.Context, contract *session.RoleContract) context.Context {
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
	if opts := callOptionsFrom(ctx); opts != nil && opts.RoleContract != nil {
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

func RoleContractAllowsTool(contract *session.RoleContract, name string) bool {
	return roleContractAllowsName(contract, name)
}

// roleToolAllowed checks every independently carried contract. Batch payload
// policy and the current CallOptions are conjunctive, never replacements.
func roleToolAllowed(ctx context.Context, name string) bool {
	for _, contract := range roleToolContractsFrom(ctx) {
		if !roleContractAllowsName(contract, name) {
			return false
		}
	}
	return true
}

func roleToolsConstrained(ctx context.Context) bool {
	for _, contract := range roleToolContractsFrom(ctx) {
		if contract == nil || contract.Validate() != nil || contract.Tools != nil || len(contract.Inherited) > 0 {
			return true
		}
	}
	return false
}

func guardRoleToolCall(ctx context.Context, name string) error {
	if roleToolAllowed(ctx, name) {
		return nil
	}
	return fmt.Errorf("%w: %q is outside the current role's allowed tools", ErrRoleToolDenied, name)
}

func pruneRoleContractTools(ctx context.Context, tools []fantasy.AgentTool) []fantasy.AgentTool {
	return filterRoleContractTools(ctx, tools)
}

func filterRoleToolNames(ctx context.Context, names []string) []string {
	if len(roleToolContractsFrom(ctx)) == 0 {
		return names
	}
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if roleToolAllowed(ctx, name) {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

func filterRoleContractTools(ctx context.Context, tools []fantasy.AgentTool) []fantasy.AgentTool {
	currentContracts := roleToolContractsFrom(ctx)
	filtered := make([]fantasy.AgentTool, 0, len(tools))
	for _, tool := range tools {
		capturedContracts := roleContractsFromTool(tool)
		if !roleToolAllowed(ctx, tool.Info().Name) || !roleContractsAllowName(capturedContracts, tool.Info().Name) {
			continue
		}
		guardContracts := cloneRoleToolContracts(capturedContracts)
		for _, active := range currentContracts {
			if !hasRoleContract(guardContracts, active) {
				guardContracts = append(guardContracts, active.Clone())
			}
		}
		if len(guardContracts) > 0 {
			filtered = append(filtered, roleGuardTool{AgentTool: tool, contracts: guardContracts})
		} else {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func hasRoleContract(contracts []*session.RoleContract, target *session.RoleContract) bool {
	for _, existing := range contracts {
		if existing.Equal(target) {
			return true
		}
	}
	return false
}

func cloneRoleToolContracts(contracts []*session.RoleContract) []*session.RoleContract {
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
	if err := guardRoleToolCall(ctx, canonical); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return t.AgentTool.Run(ctx, call)
}

func guardRoleContractError(name string) error {
	return fmt.Errorf("%w: %q is outside the current role's allowed tools", ErrRoleToolDenied, name)
}

func (t roleGuardTool) ProviderOptions() fantasy.ProviderOptions {
	return t.AgentTool.ProviderOptions()
}

func (t roleGuardTool) SetProviderOptions(options fantasy.ProviderOptions) {
	t.AgentTool.SetProviderOptions(options)
}
