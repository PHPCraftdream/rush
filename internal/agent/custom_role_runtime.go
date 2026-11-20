package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	"github.com/PHPCraftdream/rush/internal/config"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// ResolveRoleContract validates and snapshots an exact configured role at admission.
func ResolveRoleContract(cfg *config.Config, name string, inherited *session.RoleContract) (*session.RoleContract, error) {
	return modelkit.ResolveRoleContract(cfg, name, inherited)
}

// Compatibility ports for batch callers that remain in the root package.
func inheritedRoleCeilings(parent *session.RoleContract) ([][]string, error) {
	return modelkit.InheritedRoleCeilings(parent)
}

func cloneRoleContractForCall(contract *session.RoleContract) *session.RoleContract {
	return agentcall.CloneRoleContractForCall(contract)
}

func roleContractAllowsTool(contract *session.RoleContract, name string) bool {
	return modelkit.RoleContractAllowsTool(contract, name)
}

// WithSelectedRoleContract transports a batch-admitted immutable role choice.
func WithSelectedRoleContract(ctx context.Context, contract *session.RoleContract) context.Context {
	return modelkit.WithSelectedRoleContract(ctx, contract)
}

// WithCallRoleContract transports a detached immutable role snapshot through a call context.
func WithCallRoleContract(ctx context.Context, contract *session.RoleContract) context.Context {
	return modelkit.WithCallRoleContract(ctx, contract)
}

// WithRoleContractCopy returns options with a detached contract and the original execution policy.
func WithRoleContractCopy(opts *CallOptions, contract *session.RoleContract) *CallOptions {
	return modelkit.WithRoleContractCopy(opts, contract)
}
