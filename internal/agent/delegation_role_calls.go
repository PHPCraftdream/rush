package agent

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session"
)

func (c *coordinator) refreshRequestedDelegationCall(ctx context.Context, parentID string, call SessionAgentCall) (SessionAgentCall, error) {
	ctx = withCallCredentials(ctx, call.Credentials)
	if call.CallOptions != nil {
		ctx = WithCallOptions(ctx, call.CallOptions)
	}
	if call.RoleContract != nil {
		if call.CallOptions == nil {
			call.CallOptions = &CallOptions{}
		}
		call.CallOptions.RoleContract = cloneRoleContractForCall(call.RoleContract)
		ctx = WithCallOptions(ctx, call.CallOptions)
	}
	if call.CallOptions == nil || call.CallOptions.RoleContract == nil {
		if contract, contractErr := loadChildRoleContract(ctx, c.sessions, call.SessionID); contractErr != nil {
			return SessionAgentCall{}, contractErr
		} else if contract != nil {
			ctx = WithCallRoleContract(ctx, contract)
			call.CallOptions = callOptionsFrom(ctx)
		}
	}
	cfg, _ := c.cfg.Snapshot()
	selected, err := c.resolveRequestedDelegationModel(ctx, cfg, parentID, call.RequestedModelRole)
	if err != nil {
		return SessionAgentCall{}, err
	}
	if call.CallOptions == nil {
		call.CallOptions = callOptionsFrom(ctx)
	} else if call.CallOptions.RoleContract == nil {
		call.CallOptions = callOptionsFrom(ctx)
	}
	call = applyDrainModelOptions(call, selected.Model)
	call.ProviderOptions = getProviderOptions(call.SessionID, selected.Model, selected.Provider)
	call.RequestedModelSource = selected.Source
	return call, nil
}

// restoreRequestedDelegationCall validates the role without changing call authority.
func (c *coordinator) restoreRequestedDelegationCall(ctx context.Context, call SessionAgentCall) (SessionAgentCall, error) {
	if call.RoleContract != nil {
		call.CallOptions = WithRoleContractCopy(call.CallOptions, call.RoleContract)
		ctx = WithCallOptions(ctx, call.CallOptions)
	} else if call.CallOptions == nil || call.CallOptions.RoleContract == nil {
		contract, err := loadChildRoleContract(ctx, c.sessions, call.SessionID)
		if err != nil {
			return SessionAgentCall{}, err
		}
		if contract != nil {
			call.CallOptions = WithRoleContractCopy(call.CallOptions, contract)
			call.RoleContract = contract.Clone()
			ctx = WithCallOptions(ctx, call.CallOptions)
		}
	}
	if call.RequestedModelRole != "" && !call.RequestedModelRole.valid() {
		return SessionAgentCall{}, ErrAgentRoleInvalid
	}
	if call.RequestedModelRole != "" && !call.RequestedModelRole.builtin() {
		if call.CallOptions == nil || call.CallOptions.RoleContract == nil || call.CallOptions.RoleContract.Name != string(call.RequestedModelRole) {
			return SessionAgentCall{}, ErrAgentRoleUnconfigured
		}
		stored, err := loadChildRoleContract(ctx, c.sessions, call.SessionID)
		if err != nil {
			return SessionAgentCall{}, err
		}
		if stored == nil || !stored.Equal(call.CallOptions.RoleContract) {
			return SessionAgentCall{}, ErrAgentRoleMismatch
		}
		child, err := c.sessions.Get(ctx, call.SessionID)
		if err != nil {
			return SessionAgentCall{}, err
		}
		if child.ParentSessionID == "" {
			return SessionAgentCall{}, fmt.Errorf("%w: delegated role has no parent", ErrAgentRoleMismatch)
		}
		if call.Credentials == nil && c.drainCredentialBoundary(ctx, child.ID) {
			return SessionAgentCall{}, ErrCredentialBindingUnavailable
		}
		if call.SmartModel == nil {
			return c.refreshRequestedDelegationCall(ctx, child.ParentSessionID, call)
		}
		return call, nil
	}
	store, supported := c.sessions.(session.AgentModelRoleStore)
	if !supported {
		if call.RequestedModelRole != "" {
			return SessionAgentCall{}, fmt.Errorf("%w: model-role persistence unavailable", ErrAgentRoleUnconfigured)
		}
		return call, nil
	}
	saved, _, err := store.GetAgentModelRole(ctx, call.SessionID)
	if err != nil {
		return SessionAgentCall{}, err
	}
	if call.RequestedModelRole != "" && string(call.RequestedModelRole) != saved {
		return SessionAgentCall{}, ErrAgentRoleMismatch
	}
	if saved == "" {
		return call, nil
	}
	call.RequestedModelRole = RequestedModelRole(saved)
	if !call.RequestedModelRole.valid() {
		return SessionAgentCall{}, ErrAgentRoleInvalid
	}
	child, err := c.sessions.Get(ctx, call.SessionID)
	if err != nil {
		return SessionAgentCall{}, err
	}
	if child.ParentSessionID == "" {
		return SessionAgentCall{}, fmt.Errorf("%w: delegated role has no parent", ErrAgentRoleMismatch)
	}
	if call.Credentials == nil && c.drainCredentialBoundary(ctx, child.ID) {
		return SessionAgentCall{}, ErrCredentialBindingUnavailable
	}
	return c.refreshRequestedDelegationCall(ctx, child.ParentSessionID, call)
}

func (c *coordinator) refuseLostRequestedRoleDriver(ctx context.Context, id string) error {
	store, supported := c.sessions.(session.AgentModelRoleStore)
	if !supported {
		return nil
	}
	role, _, err := store.GetAgentModelRole(ctx, id)
	if err != nil {
		return err
	}
	if role != "" {
		return fmt.Errorf("%w: delegated model-role driver unavailable", ErrAgentRoleUnconfigured)
	}
	return nil
}
