package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchRoleAdmissionSnapshotsEntireTreeAndInheritance(t *testing.T) {
	tools := []string{"view"}
	cfg := &config.Config{Roles: map[string]config.CustomRole{
		"reader": {SelectedModel: config.SelectedModel{Provider: "p", Model: "old"}, Tools: &tools},
		"writer": {SelectedModel: config.SelectedModel{Provider: "p", Model: "writer"}},
	}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"p": {ID: "p"}})}
	parentTools := []string{"view", "agent"}
	parent := &session.RoleContract{Version: session.RoleContractVersion, Name: "parent", Model: session.ModelCfg{Provider: "p", Model: "p-model"}, Tools: &parentTools}
	spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{
		{Task: batchCustomRoleTask("reader")},
		{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{{Task: batchCustomRoleTask("writer")}}}},
	}}}
	admitted, err := admitBatchRoleContracts(cfg, parent, spec)
	require.NoError(t, err)
	first := batchPayloadAt(t, admitted.Batch.Items[0])
	second := batchPayloadAt(t, admitted.Batch.Items[1].Batch.Items[0])
	require.Equal(t, "reader", first.SelectedRoleContract.Name)
	require.Equal(t, "old", first.SelectedRoleContract.Model.Model)
	require.Equal(t, [][]string{parentTools}, first.SelectedRoleContract.Inherited)
	require.Nil(t, second.SelectedRoleContract.Tools, "omitted role tools remain unrestricted by themselves")
	require.Equal(t, [][]string{parentTools}, second.SelectedRoleContract.Inherited)

	// A post-admission config edit/removal cannot widen or replace the selected role.
	cfg.Roles["reader"] = config.CustomRole{SelectedModel: config.SelectedModel{Provider: "p", Model: "new"}}
	queued := batchPayloadAt(t, admitted.Batch.Items[0])
	require.Equal(t, "old", queued.SelectedRoleContract.Model.Model)
}

func TestBatchRoleAdmissionRejectsUnknownInvalidAndForgedSnapshots(t *testing.T) {
	cfg := &config.Config{Roles: map[string]config.CustomRole{"known": {SelectedModel: config.SelectedModel{Provider: "p", Model: "m"}}}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"p": {ID: "p"}})}
	for _, tc := range []struct {
		name     string
		input    string
		contract *session.RoleContract
	}{
		{"unknown", `{"role":"missing"}`, nil},
		{"invalid builtin slot", `{"role":"reviewer"}`, nil},
		{"forged snapshot", `{"role":"known"}`, &session.RoleContract{Version: session.RoleContractVersion, Name: "known", Model: session.ModelCfg{Provider: "p", Model: "forged"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(batchLeafPayload{Tool: AgentToolName, Input: json.RawMessage(tc.input), SelectedRoleContract: tc.contract})
			require.NoError(t, err)
			_, err = admitBatchRoleContracts(cfg, nil, jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: raw}})
			switch tc.name {
			case "unknown", "invalid builtin slot":
				require.ErrorIs(t, err, ErrAgentRoleUnconfigured)
			case "forged snapshot":
				require.ErrorIs(t, err, ErrAgentRoleMismatch)
			}
		})
	}
}

func TestBatchRoleAdmissionRequiresValidRoleInputBeforeResources(t *testing.T) {
	for _, input := range []string{`{"role":null}`, `{"role":""}`, `{"role":"custom/name"}`, `{"role":7}`} {
		t.Run(input, func(t *testing.T) {
			spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Items: []jobbatch.Spec{{Task: batchCLITaskSpec(AgentToolName, json.RawMessage(input))}}}}
			_, err := AdmitBatchRoleContracts(context.Background(), nil, spec)
			require.ErrorIs(t, err, ErrAgentRoleInvalid)
		})
	}
}

func batchCustomRoleTask(name string) *jobbatch.TaskSpec {
	return batchCLITaskSpec(AgentToolName, json.RawMessage(`{"role":`+batchRoleJSONString(name)+`,"prompt":"work"}`))
}

func batchCLITaskSpec(tool string, input json.RawMessage) *jobbatch.TaskSpec {
	raw, _ := json.Marshal(BatchCLITask{Tool: tool, Input: input})
	return &jobbatch.TaskSpec{Payload: raw}
}

func batchPayloadAt(t *testing.T, spec jobbatch.Spec) batchLeafPayload {
	t.Helper()
	var p batchLeafPayload
	require.NoError(t, json.Unmarshal(spec.Task.Payload, &p))
	return p
}

func batchRoleJSONString(value string) string { raw, _ := json.Marshal(value); return string(raw) }
