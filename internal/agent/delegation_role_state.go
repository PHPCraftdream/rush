package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session"
)

// resolveDelegationRequestedRole is called only after resume ownership is checked.
func resolveDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested RequestedModelRole) (RequestedModelRole, error) {
	if requested != "" && !requested.valid() {
		return "", ErrAgentRoleInvalid
	}
	store, supported := sessions.(session.AgentModelRoleStore)
	if childID == "" {
		if requested != "" && !supported {
			return "", fmt.Errorf("%w: model-role persistence unavailable", ErrAgentRoleUnconfigured)
		}
		return requested, nil
	}
	if !supported {
		return "", fmt.Errorf("%w: resumed model-role persistence unavailable", ErrAgentRoleUnconfigured)
	}
	saved, _, err := store.GetAgentModelRole(ctx, childID)
	if err != nil {
		return "", err
	}
	role := RequestedModelRole(saved) // Missing pre-role rows retain legacy auto selection.
	if role != "" && !role.valid() {
		return "", fmt.Errorf("%w: invalid saved model role", ErrAgentRoleUnconfigured)
	}
	if requested != "" && requested != role {
		return "", ErrAgentRoleMismatch
	}
	return role, nil
}

func bindDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested RequestedModelRole) error {
	store, supported := sessions.(session.AgentModelRoleStore)
	if !supported {
		if requested != "" {
			return fmt.Errorf("%w: model-role persistence unavailable", ErrAgentRoleUnconfigured)
		}
		return nil
	}
	if err := store.BindAgentModelRole(ctx, childID, string(requested)); err != nil {
		if errors.Is(err, session.ErrAgentModelRoleMismatch) {
			return ErrAgentRoleMismatch
		}
		return err
	}
	return nil
}
