package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter/delegation"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

func resolveDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested RequestedModelRole) (RequestedModelRole, error) {
	return delegation.ResolveDelegationRequestedRole(ctx, sessions, childID, requested)
}

func bindDelegationRoleContract(ctx context.Context, sessions session.Service, childID string, contract *session.RoleContract) error {
	return delegation.BindDelegationRoleContract(ctx, sessions, childID, contract)
}

func bindDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested RequestedModelRole) error {
	return delegation.BindDelegationRequestedRole(ctx, sessions, childID, requested)
}
