// Root-call admission binding for custom role contracts. A new logical root call
// replaces the current row; continuations retain the same LogicalCallID.
package agent

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session"
)

func bindRootRoleContract(ctx context.Context, sessions session.Service, call SessionAgentCall) error {
	var contract *session.RoleContract
	if call.CallOptions != nil && call.CallOptions.RoleContract != nil {
		contract = cloneRoleContractForCall(call.CallOptions.RoleContract)
	}
	if call.RoleContract != nil {
		if contract != nil && !contract.Equal(call.RoleContract) {
			return ErrAgentRoleMismatch
		}
		contract = cloneRoleContractForCall(call.RoleContract)
	}
	if contract != nil && call.LogicalCallID == "" {
		return fmt.Errorf("%w: root role contract requires a logical call id", ErrAgentRoleUnconfigured)
	}
	if call.LogicalCallID == "" || sessions == nil {
		if contract != nil {
			return fmt.Errorf("%w: root role contract persistence unavailable", ErrAgentRoleUnconfigured)
		}
		return nil
	}
	store, ok := sessions.(session.SessionRootRoleContractStore)
	if !ok {
		if contract != nil {
			return fmt.Errorf("%w: root role contract persistence unavailable", ErrAgentRoleUnconfigured)
		}
		return nil
	}
	return store.BindRootRoleContract(ctx, call.SessionID, call.LogicalCallID, contract)
}

func restoreRootRoleContract(ctx context.Context, sessions session.Service, call SessionAgentCall) (SessionAgentCall, error) {
	if call.LogicalCallID == "" {
		return call, nil
	}
	if call.RoleContract != nil && (call.CallOptions == nil || call.CallOptions.RoleContract == nil) {
		call.CallOptions = WithRoleContractCopy(call.CallOptions, call.RoleContract)
	}
	if call.CallOptions != nil && call.CallOptions.RoleContract != nil {
		contract := call.CallOptions.RoleContract
		if call.RoleContract != nil && !call.RoleContract.Equal(contract) {
			return SessionAgentCall{}, ErrAgentRoleMismatch
		}
		if err := contract.Validate(); err != nil {
			return SessionAgentCall{}, fmt.Errorf("%w: invalid supplied root role contract: %v", ErrAgentRoleUnconfigured, err)
		}
		call.RoleContract = contract.Clone()
	}
	store, ok := sessions.(session.SessionRootRoleContractStore)
	if !ok {
		return call, nil
	}
	logicalID, contract, found, err := store.GetRootRoleContract(ctx, call.SessionID)
	if err != nil {
		return SessionAgentCall{}, err
	}
	if !found || logicalID != call.LogicalCallID {
		return call, nil
	}
	if call.CallOptions != nil && call.CallOptions.RoleContract != nil && !call.CallOptions.RoleContract.Equal(contract) {
		return SessionAgentCall{}, ErrAgentRoleMismatch
	}
	if contract == nil {
		if call.RoleContract != nil || call.CallOptions != nil && call.CallOptions.RoleContract != nil {
			return SessionAgentCall{}, ErrAgentRoleMismatch
		}
		return call, nil
	}
	if err := contract.Validate(); err != nil {
		return SessionAgentCall{}, fmt.Errorf("%w: invalid root role contract: %v", ErrAgentRoleUnconfigured, err)
	}
	call.CallOptions = WithRoleContractCopy(call.CallOptions, contract)
	call.RoleContract = contract.Clone()
	return call, nil
}
