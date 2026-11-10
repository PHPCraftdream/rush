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
	contractStore, contractSupported := sessions.(session.SessionRoleContractStore)
	if contractSupported {
		contract, found, err := contractStore.GetSessionRoleContract(ctx, childID)
		if err != nil {
			return "", err
		}
		if found {
			if requested != "" && requested != RequestedModelRole(contract.Name) {
				return "", ErrAgentRoleMismatch
			}
			if contract.Name == "" {
				return requested, nil
			}
			return RequestedModelRole(contract.Name), nil
		}
	}
	if !supported {
		return "", fmt.Errorf("%w: resumed model-role persistence unavailable", ErrAgentRoleUnconfigured)
	}
	saved, _, err := store.GetAgentModelRole(ctx, childID)
	if err != nil {
		return "", err
	}
	role := RequestedModelRole(saved)
	if role != "" && !role.valid() {
		return "", fmt.Errorf("%w: invalid saved model role", ErrAgentRoleUnconfigured)
	}
	if requested != "" && requested != role {
		return "", ErrAgentRoleMismatch
	}
	return role, nil
}

func bindDelegationRoleContract(ctx context.Context, sessions session.Service, childID string, contract *session.RoleContract) error {
	store, supported := sessions.(session.SessionRoleContractStore)
	if !supported {
		return fmt.Errorf("%w: role contract persistence unavailable", ErrAgentRoleUnconfigured)
	}
	if err := store.BindSessionRoleContract(ctx, childID, contract); err != nil {
		if errors.Is(err, session.ErrSessionRoleContractMismatch) {
			return ErrAgentRoleMismatch
		}
		return err
	}
	return nil
}

func bindDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested RequestedModelRole) error {
	if requested != "" && !requested.builtin() {
		store, supported := sessions.(session.SessionRoleContractStore)
		if !supported {
			return fmt.Errorf("%w: custom role contract persistence unavailable", ErrAgentRoleUnconfigured)
		}
		contract, found, err := store.GetSessionRoleContract(ctx, childID)
		if err != nil {
			return err
		}
		if !found || contract.Name != string(requested) {
			return fmt.Errorf("%w: custom role contract missing before role binding", ErrAgentRoleUnconfigured)
		}
	}
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
