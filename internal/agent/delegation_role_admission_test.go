package agent

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAgentRoleAdmissionRejectsInvalidValuesAcrossBatchAdapters(t *testing.T) {
	t.Parallel()
	inv := BatchInvocation{SessionID: "parent", CallID: "call", RegistryID: "registry", Policy: batchLeafPolicyContext{RegistryID: "registry", RestrictedToolset: AgentToolName}}
	for _, value := range []string{`null`, `""`, `"unknown"`, `false`} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			input := json.RawMessage(`{"prompt":"p","role":` + value + `}`)
			_, err := (BatchExternalSpec{Tool: AgentToolName, Input: input}).Spec()
			require.ErrorIs(t, err, ErrAgentRoleInvalid)
			_, err = modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: AgentToolName, Input: input}}}, inv, modelRegistry(AgentToolName))
			require.ErrorIs(t, err, ErrAgentRoleInvalid)
			_, err = batchOneItemSpec(inv, fantasy.ToolCall{Name: AgentToolName}, input)
			require.ErrorIs(t, err, ErrAgentRoleInvalid)
			var inputErr batchToolInputError
			require.ErrorAs(t, err, &inputErr)
		})
	}
}

func TestRoleFieldsOnOtherToolsAreNotAgentControl(t *testing.T) {
	t.Parallel()
	_, err := (BatchExternalSpec{Tool: "view", Input: json.RawMessage(`{"path":"x","role":null}`)}).Spec()
	require.NoError(t, err)
}

func TestRequestedRoleDurableRoundTripDoesNotReplacePolicyRole(t *testing.T) {
	t.Parallel()
	original := SessionAgentCall{
		SessionID: "child", RequestedModelRole: "reviewer", RequestedModelSource: hbSourceSessionOverride,
		MaxCost:     9,
		CallOptions: &CallOptions{ModelRole: config.SelectedModelTypeFast, DisableSubAgents: true, MaxAgentDepth: 1, AgentDepthSet: true, MaxCost: 9},
	}
	data := ToSessionAgentCallData(original)
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var decoded session.SessionAgentCallData
	require.NoError(t, json.Unmarshal(raw, &decoded))
	got, err := FromSessionAgentCallData(decoded)
	require.NoError(t, err)
	require.Equal(t, original.RequestedModelRole, got.RequestedModelRole)
	require.Equal(t, original.RequestedModelSource, got.RequestedModelSource)
	require.Equal(t, config.SelectedModelTypeFast, got.CallOptions.ModelRole)
	require.True(t, got.CallOptions.DisableSubAgents)
	require.Equal(t, 1, got.CallOptions.MaxAgentDepth)
	require.EqualValues(t, 9, got.MaxCost, "durable budgets live on the call, not CallOptionsSpec")
	decoded.RequestedModelRole = "unknown"
	_, err = FromSessionAgentCallData(decoded)
	require.ErrorIs(t, err, ErrAgentRoleInvalid)
}
