package agent

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAgentRoleAdmissionRejectsInvalidValuesAcrossBatchAdapters(t *testing.T) {
	t.Parallel()
	inv := BatchInvocation{SessionID: "parent", CallID: "call", RegistryID: "registry", Policy: batchLeafPolicyContext{RegistryID: "registry", RestrictedToolset: AgentToolName}}
	for _, value := range []string{`null`, `""`, `"custom/name"`, `false`} {
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
	decoded.RequestedModelRole = "analyst/child"
	_, err = FromSessionAgentCallData(decoded)
	require.ErrorIs(t, err, ErrAgentRoleInvalid)
}

func TestAgentRoleBatchAdaptersAcceptConfiguredCustomNames(t *testing.T) {
	providers := csync.NewMap[string, config.ProviderConfig]()
	providers.Set("p", config.ProviderConfig{ID: "p"})
	cfg := &config.Config{
		Providers: providers,
		Roles: map[string]config.CustomRole{
			"auto":    {SelectedModel: config.SelectedModel{Provider: "p", Model: "automatic-custom-model"}},
			"unknown": {SelectedModel: config.SelectedModel{Provider: "p", Model: "unknown-custom-model"}},
		},
	}
	inv := BatchInvocation{
		SessionID: "parent", CallID: "call", RegistryID: "registry",
		Policy:     batchLeafPolicyContext{RegistryID: "registry", RestrictedToolset: AgentToolName},
		roleConfig: cfg,
	}
	for _, role := range []string{"auto", "unknown"} {
		t.Run(role, func(t *testing.T) {
			input := json.RawMessage(`{"prompt":"p","role":"` + role + `"}`)
			_, err := (BatchExternalSpec{Tool: AgentToolName, Input: input}).Spec()
			require.NoError(t, err, "external parsing checks syntax, not configured definitions")
			modelSpec, err := modelBatchSpec(batchModelSpec{Mode: "parallel", Items: []batchModelItem{{Tool: AgentToolName, Input: input}}}, inv, modelRegistry(AgentToolName))
			require.NoError(t, err)
			modelPayload := batchPayloadAt(t, modelSpec.Batch.Items[0])
			require.Equal(t, role, modelPayload.SelectedRoleContract.Name)
			direct, err := batchOneItemSpec(inv, fantasy.ToolCall{Name: AgentToolName}, input)
			require.NoError(t, err)
			directPayload := batchPayloadAt(t, direct)
			require.Equal(t, role, directPayload.SelectedRoleContract.Name)
		})
	}
}

func TestAgentRoleBatchAdmissionRejectsUnknownCustomBeforeEffects(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	cfg := &config.Config{Roles: map[string]config.CustomRole{}, Providers: csync.NewMap[string, config.ProviderConfig]()}
	ctx := WithParentRoleConfigSnapshot(t.Context(), cfg)
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{
		batchRoleAdmissionTask(json.RawMessage(`{"prompt":"p","role":"unknown"}`)),
	}}}
	_, err := AdmitBatchRoleContracts(ctx, nil, spec)
	require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
}

func batchRoleAdmissionTask(input json.RawMessage) jobbatch.Spec {
	return jobbatch.Spec{Task: batchCLITaskSpec(AgentToolName, input)}
}
