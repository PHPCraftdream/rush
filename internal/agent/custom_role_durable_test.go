package agent

import (
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCustomRoleContractDurableCallOptionsRoundTrip(t *testing.T) {
	contract, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)
	data := ToSessionAgentCallData(SessionAgentCall{SessionID: "custom-root", LogicalCallID: "logical-70", CallOptions: &CallOptions{RoleContract: contract}})
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.NotNil(t, decoded.CallOptionsSpec)
	require.Equal(t, session.CallOptionsSpecVersion, decoded.CallOptionsSpec.Version)
	require.NotNil(t, decoded.CallOptionsSpec.RoleContract)
	got, err := FromSessionAgentCallData(decoded)
	require.NoError(t, err)
	require.NotNil(t, got.CallOptions.RoleContract)
	require.True(t, contract.Equal(got.CallOptions.RoleContract))
	decoded.CallOptionsSpec.Version = 2
	_, err = FromSessionAgentCallData(decoded)
	require.ErrorIs(t, err, ErrCallOptionsSpecVersionUnsupported)
}

func TestCustomRoleContractStandaloneDurableCallRequiresV3CallOptions(t *testing.T) {
	contract, err := ResolveRoleContract(customRoleResolverConfig(), "analyst", nil)
	require.NoError(t, err)
	data := ToSessionAgentCallData(SessionAgentCall{SessionID: "custom-root", LogicalCallID: "logical-71", RoleContract: contract})
	require.NotNil(t, data.CallOptionsSpec)
	require.Equal(t, session.CallOptionsSpecVersion, data.CallOptionsSpec.Version)
	require.NotNil(t, data.CallOptionsSpec.RoleContract)
	require.True(t, contract.Equal(data.CallOptionsSpec.RoleContract))

	got, err := FromSessionAgentCallData(data)
	require.NoError(t, err)
	require.NotNil(t, got.CallOptions)
	require.NotNil(t, got.RoleContract)
	require.True(t, contract.Equal(got.RoleContract))

	data.CallOptionsSpec = nil
	_, err = FromSessionAgentCallData(data)
	require.ErrorIs(t, err, ErrCallOptionsSpecVersionUnsupported)
}

func TestCustomRoleContinuationRejectsForgedBroaderStoredContract(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	child, err := coord.sessions.CreateTaskSession(t.Context(), "forged-custom-contract", parent, "custom child")
	require.NoError(t, err)
	coord.cfg.Config().Roles = map[string]config.CustomRole{
		"analyst": {SelectedModel: config.SelectedModel{Provider: "role-fixture", Model: "smart-model"}, Tools: &[]string{"view"}},
	}
	contract, err := ResolveRoleContract(coord.cfg.Config(), "analyst", nil)
	require.NoError(t, err)
	coord.markCredentialedRoot(parent)
	store := coord.sessions.(session.SessionRoleContractStore)
	require.NoError(t, store.BindSessionRoleContract(t.Context(), child.ID, contract))
	wider := contract.Clone()
	wider.Tools = nil
	call := SessionAgentCall{SessionID: child.ID, RequestedModelRole: "analyst", CallOptions: &CallOptions{RoleContract: wider}}
	_, err = coord.restoreRequestedDelegationCall(t.Context(), call)
	require.ErrorIs(t, err, ErrAgentRoleMismatch)
	missingBundle := SessionAgentCall{SessionID: child.ID, RequestedModelRole: "analyst", RoleContract: contract}
	_, err = coord.restoreRequestedDelegationCall(t.Context(), missingBundle)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)

	// A same-name persisted binding that is wider than the call's admission
	// snapshot must fail closed too; the durable row cannot silently replace
	// the narrower admitted ceiling during continuation.
	widerChild, err := coord.sessions.CreateTaskSession(t.Context(), "forged-wider-stored-custom-contract", parent, "custom child")
	require.NoError(t, err)
	require.NoError(t, store.BindSessionRoleContract(t.Context(), widerChild.ID, wider))
	boundCall := SessionAgentCall{
		SessionID:          widerChild.ID,
		RequestedModelRole: "analyst",
		CallOptions:        &CallOptions{RoleContract: contract.Clone()},
	}
	_, err = coord.restoreRequestedDelegationCall(t.Context(), boundCall)
	require.ErrorIs(t, err, ErrAgentRoleMismatch)
}

func TestCustomRoleDurableContractRejectsRemovedRoleOnNextResolve(t *testing.T) {
	cfg := customRoleResolverConfig()
	contract, err := ResolveRoleContract(cfg, "analyst", nil)
	require.NoError(t, err)
	data := ToSessionAgentCallData(SessionAgentCall{SessionID: "removed-role", RoleContract: contract})
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	restored, err := FromSessionAgentCallData(decoded)
	require.NoError(t, err)
	delete(cfg.Roles, "analyst")
	_, _, err = currentOrSnapshotRoleModel(cfg, restored.RoleContract)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	require.True(t, contract.Equal(restored.RoleContract), "refusal must preserve the durable binding")
}

func TestCustomRoleResolverRejectsUnavailableExactKey(t *testing.T) {
	cfg := customRoleResolverConfig()
	_, err := ResolveRoleContract(cfg, "missing", nil)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
	_, err = ResolveRoleContract(cfg, "Analyst", nil)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
}
