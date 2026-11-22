package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter/delegation"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

func bindRootRoleContract(ctx context.Context, sessions session.Service, call SessionAgentCall) error {
	return delegation.BindRootRoleContract(ctx, sessions, call)
}

func restoreRootRoleContract(ctx context.Context, sessions session.Service, call SessionAgentCall) (SessionAgentCall, error) {
	return delegation.RestoreRootRoleContract(ctx, sessions, call)
}
