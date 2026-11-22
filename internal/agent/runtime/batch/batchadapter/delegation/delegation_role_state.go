package delegation

import (
	"context"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// ResolveDelegationRequestedRole restores a role only after resume ownership has been checked.
func ResolveDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested agentcall.RequestedModelRole) (agentcall.RequestedModelRole, error) {
	if requested != "" && !requested.Valid() {
		return "", agentcall.ErrAgentRoleInvalid
	}
	store, supported := sessions.(session.AgentModelRoleStore)
	if childID == "" {
		if requested != "" && !supported {
			return "", fmt.Errorf("%w: model-role persistence unavailable", agentcall.ErrAgentRoleUnconfigured)
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
			if requested != "" && requested != agentcall.RequestedModelRole(contract.Name) {
				return "", agentcall.ErrAgentRoleMismatch
			}
			if contract.Name == "" {
				return requested, nil
			}
			return agentcall.RequestedModelRole(contract.Name), nil
		}
	}
	if !supported {
		return "", fmt.Errorf("%w: resumed model-role persistence unavailable", agentcall.ErrAgentRoleUnconfigured)
	}
	saved, _, err := store.GetAgentModelRole(ctx, childID)
	if err != nil {
		return "", err
	}
	role := agentcall.RequestedModelRole(saved)
	if role != "" && !role.Valid() {
		return "", fmt.Errorf("%w: invalid saved model role", agentcall.ErrAgentRoleUnconfigured)
	}
	if requested != "" && requested != role {
		return "", agentcall.ErrAgentRoleMismatch
	}
	return role, nil
}

func BindDelegationRoleContract(ctx context.Context, sessions session.Service, childID string, contract *session.RoleContract) error {
	store, supported := sessions.(session.SessionRoleContractStore)
	if !supported {
		return fmt.Errorf("%w: role contract persistence unavailable", agentcall.ErrAgentRoleUnconfigured)
	}
	if err := store.BindSessionRoleContract(ctx, childID, contract); err != nil {
		if errors.Is(err, session.ErrSessionRoleContractMismatch) {
			return agentcall.ErrAgentRoleMismatch
		}
		return err
	}
	return nil
}

func BindDelegationRequestedRole(ctx context.Context, sessions session.Service, childID string, requested agentcall.RequestedModelRole) error {
	if requested != "" && !requested.Builtin() {
		store, supported := sessions.(session.SessionRoleContractStore)
		if !supported {
			return fmt.Errorf("%w: custom role contract persistence unavailable", agentcall.ErrAgentRoleUnconfigured)
		}
		contract, found, err := store.GetSessionRoleContract(ctx, childID)
		if err != nil {
			return err
		}
		if !found || contract.Name != string(requested) {
			return fmt.Errorf("%w: custom role contract missing before role binding", agentcall.ErrAgentRoleUnconfigured)
		}
	}
	store, supported := sessions.(session.AgentModelRoleStore)
	if !supported {
		if requested != "" {
			return fmt.Errorf("%w: model-role persistence unavailable", agentcall.ErrAgentRoleUnconfigured)
		}
		return nil
	}
	if err := store.BindAgentModelRole(ctx, childID, string(requested)); err != nil {
		if errors.Is(err, session.ErrAgentModelRoleMismatch) {
			return agentcall.ErrAgentRoleMismatch
		}
		return err
	}
	return nil
}
