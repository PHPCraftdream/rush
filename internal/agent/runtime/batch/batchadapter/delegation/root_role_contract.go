package delegation

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

func BindRootRoleContract(ctx context.Context, sessions session.Service, call agentcall.SessionAgentCall) error {
	var contract *session.RoleContract
	if call.CallOptions != nil && call.CallOptions.RoleContract != nil {
		contract = agentcall.CloneRoleContractForCall(call.CallOptions.RoleContract)
	}
	if call.RoleContract != nil {
		if contract != nil && !contract.Equal(call.RoleContract) {
			return agentcall.ErrAgentRoleMismatch
		}
		contract = agentcall.CloneRoleContractForCall(call.RoleContract)
	}
	if contract != nil && call.LogicalCallID == "" {
		return fmt.Errorf("%w: root role contract requires a logical call id", agentcall.ErrAgentRoleUnconfigured)
	}
	if call.LogicalCallID == "" || sessions == nil {
		if contract != nil {
			return fmt.Errorf("%w: root role contract persistence unavailable", agentcall.ErrAgentRoleUnconfigured)
		}
		return nil
	}
	store, ok := sessions.(session.SessionRootRoleContractStore)
	if !ok {
		if contract != nil {
			return fmt.Errorf("%w: root role contract persistence unavailable", agentcall.ErrAgentRoleUnconfigured)
		}
		return nil
	}
	return store.BindRootRoleContract(ctx, call.SessionID, call.LogicalCallID, contract)
}

func RestoreRootRoleContract(ctx context.Context, sessions session.Service, call agentcall.SessionAgentCall) (agentcall.SessionAgentCall, error) {
	if call.LogicalCallID == "" {
		return call, nil
	}
	if call.RoleContract != nil && (call.CallOptions == nil || call.CallOptions.RoleContract == nil) {
		call.CallOptions = modelkit.WithRoleContractCopy(call.CallOptions, call.RoleContract)
	}
	if call.CallOptions != nil && call.CallOptions.RoleContract != nil {
		contract := call.CallOptions.RoleContract
		if call.RoleContract != nil && !call.RoleContract.Equal(contract) {
			return agentcall.SessionAgentCall{}, agentcall.ErrAgentRoleMismatch
		}
		if err := contract.Validate(); err != nil {
			return agentcall.SessionAgentCall{}, fmt.Errorf("%w: invalid supplied root role contract: %v", agentcall.ErrAgentRoleUnconfigured, err)
		}
		call.RoleContract = contract.Clone()
	}
	store, ok := sessions.(session.SessionRootRoleContractStore)
	if !ok {
		return call, nil
	}
	logicalID, contract, found, err := store.GetRootRoleContract(ctx, call.SessionID)
	if err != nil {
		return agentcall.SessionAgentCall{}, err
	}
	if !found || logicalID != call.LogicalCallID {
		return call, nil
	}
	if call.CallOptions != nil && call.CallOptions.RoleContract != nil && !call.CallOptions.RoleContract.Equal(contract) {
		return agentcall.SessionAgentCall{}, agentcall.ErrAgentRoleMismatch
	}
	if contract == nil {
		if call.RoleContract != nil || call.CallOptions != nil && call.CallOptions.RoleContract != nil {
			return agentcall.SessionAgentCall{}, agentcall.ErrAgentRoleMismatch
		}
		return call, nil
	}
	if err := contract.Validate(); err != nil {
		return agentcall.SessionAgentCall{}, fmt.Errorf("%w: invalid root role contract: %v", agentcall.ErrAgentRoleUnconfigured, err)
	}
	call.CallOptions = modelkit.WithRoleContractCopy(call.CallOptions, contract)
	call.RoleContract = contract.Clone()
	return call, nil
}
