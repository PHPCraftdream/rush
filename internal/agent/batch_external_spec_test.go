package agent

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestExternalBatchDepthPreservesParentBudget(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{AgentToolName, "agentic_fetch"} {
		for _, depth := range []int{0, 1, DefaultAgentDepth, MaxAgentDepthForResume} {
			t.Run(fmt.Sprintf("%s-%d", tool, depth), func(t *testing.T) {
				t.Parallel()
				leaf := BatchExternalSpec{Tool: tool, Input: json.RawMessage(`{"prompt":"p"}`)}
				spec, err := (BatchExternalSpec{Items: []BatchExternalSpec{
					{Items: []BatchExternalSpec{leaf, leaf}},
				}}).SpecWithAgentDepth(depth)
				require.NoError(t, err)
				for _, item := range spec.Batch.Items[0].Batch.Items {
					var payload batchExternalDepthPayload
					require.NoError(t, json.Unmarshal(item.Task.Payload, &payload))
					require.Equal(t, tool, payload.Tool)
					require.Equal(t, depth, payload.AgentDepth, "wrappers and siblings must not consume an edge before dispatch")
				}
			})
		}
	}
}

func TestParseBatchExternalSpecJSON(t *testing.T) {
	spec, err := ParseBatchExternalSpecJSON([]byte(`{"mode":"sequential","items":[{"tool":"agent","input":{"prompt":"p"}}]}`))
	require.NoError(t, err)
	require.NotNil(t, spec.Batch)
	require.Equal(t, jobbatch.ModeSequential, spec.Batch.Mode)
	require.Equal(t, jobbatch.OnFailStop, spec.Batch.OnFail, "a sequential batch defaults to on_fail stop")
	require.Equal(t, jobbatch.NotifyAll, spec.Batch.Notify)
	require.Len(t, spec.Batch.Items, 1)
	require.NotNil(t, spec.Batch.Items[0].Task)

	spec, err = ParseBatchExternalSpecJSON([]byte(`{"items":[]}`))
	require.NoError(t, err)
	require.Equal(t, jobbatch.ModeParallel, spec.Batch.Mode, "mode defaults to parallel")

	depthSpec, err := ParseBatchExternalSpecJSON([]byte(`{"max_agent_depth":2,"items":[{"tool":"agent","input":{"prompt":"p"}}]}`))
	require.NoError(t, err)
	var depthPayload batchExternalDepthPayload
	require.NoError(t, json.Unmarshal(depthSpec.Batch.Items[0].Task.Payload, &depthPayload))
	require.Equal(t, 2, depthPayload.AgentDepth)
	narrowed, err := ParseBatchExternalSpecJSON([]byte(`{"max_agent_depth":2,"items":[{"items":[{"tool":"agent","input":{"prompt":"p"}}]}]}`))
	require.NoError(t, err)
	var nestedPayload batchExternalDepthPayload
	require.NoError(t, json.Unmarshal(narrowed.Batch.Items[0].Batch.Items[0].Task.Payload, &nestedPayload))
	require.Equal(t, 2, nestedPayload.AgentDepth, "batch wrappers do not consume delegation edges")
	leafSpec, err := ParseBatchExternalSpecJSON([]byte(`{"max_agent_depth":1,"tool":"agent","input":{"prompt":"p"}}`))
	require.NoError(t, err)
	var singlePayload batchExternalDepthPayload
	require.NoError(t, json.Unmarshal(leafSpec.Task.Payload, &singlePayload))
	require.Equal(t, 1, singlePayload.AgentDepth, "a leaf carries its parent's remaining budget")
	_, err = ParseBatchExternalSpecJSON([]byte(`{"max_agent_depth":0,"items":[{"tool":"agent","input":{"prompt":"p"}}]}`))
	require.NoError(t, err)
	_, err = ParseBatchExternalSpecJSON([]byte(`{"max_agent_depth":33,"items":[]}`))
	require.Error(t, err)
	_, err = ParseBatchExternalSpecJSONWithAgentDepth([]byte(`{"max_agent_depth":3,"items":[]}`), 2)
	require.ErrorContains(t, err, "trusted caller cap")
	_, err = ParseBatchExternalSpecJSON([]byte(`{"max_agent_depth":1,"items":[{"max_agent_depth":2,"items":[]}]}`))
	require.Error(t, err)
	for name, raw := range map[string]string{
		"unknown field":         `{"mode":"parallel","bogus":1}`,
		"trailing value":        `{"mode":"parallel"} {"mode":"parallel"}`,
		"task mixed with batch": `{"tool":"agent","mode":"parallel"}`,
		"task with items":       `{"tool":"agent","items":[{"tool":"agent"}]}`,
		"parallel on_fail":      `{"mode":"parallel","on_fail":"stop"}`,
		"nested bad leaf":       `{"items":[{"tool":"agent","mode":"parallel"}]}`,
		"not json":              `mode: parallel`,
	} {
		_, err = ParseBatchExternalSpecJSON([]byte(raw))
		require.Error(t, err, name)
	}
}
