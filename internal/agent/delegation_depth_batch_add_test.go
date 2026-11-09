package agent

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBatchAddPropagatesLeafAdmissionErrors(t *testing.T) {
	t.Parallel()
	inv := BatchInvocation{
		SessionID: "owner", RegistryID: "registry", CallID: "add-call",
		AgentDepth: DefaultAgentDepth, AgentDepthSet: true,
		Policy: batchLeafPolicyContext{RegistryID: "registry", RestrictedToolset: "view-only"},
	}
	for _, item := range []batchModelItem{
		{Tool: "edit", Input: json.RawMessage(`{}`)},
		{Tool: "batch_stop", Input: json.RawMessage(`{}`)},
	} {
		t.Run(item.Tool, func(t *testing.T) {
			t.Parallel()
			nodes, err := batchModelSpecNodes([]batchModelItem{item}, inv, modelRegistry("view"), nil)
			require.Error(t, err, "leaf admission errors must not be shadowed before the add is committed")
			require.Nil(t, nodes)
		})
	}
}
