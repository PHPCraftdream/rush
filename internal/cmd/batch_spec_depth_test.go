package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestBatchSpecPreservesStandaloneRootDepth(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"json", "ktav"} {
		for _, depth := range []int{0, 1, agent.DefaultAgentDepth, agent.MaxAgentDepthForResume} {
			t.Run(fmt.Sprintf("%s-%d", format, depth), func(t *testing.T) {
				t.Parallel()
				raw := fmt.Sprintf(`{"max_agent_depth":%d,"items":[{"tool":"agent","input":{"prompt":"p"}}]}`, depth)
				if format == "ktav" {
					raw = fmt.Sprintf("max_agent_depth: %d\nitems: [{ tool: agent, input: { prompt: p } }]\n", depth)
				}
				cmd := &cobra.Command{}
				cmd.Flags().String("spec", "-", "")
				cmd.SetIn(strings.NewReader(raw))
				spec, got, err := parseBatchSpecWithDepth(cmd)
				require.NoError(t, err)
				require.Equal(t, depth, got, "the standalone owner must use the explicit root budget")
				require.NotNil(t, spec.Batch)
				require.Len(t, spec.Batch.Items, 1)
				var payload agent.BatchCLITask
				require.NoError(t, json.Unmarshal(spec.Batch.Items[0].Task.Payload, &payload))
				require.NotNil(t, payload.AgentDepth)
				require.Equal(t, depth, *payload.AgentDepth, "the leaf carries the parent budget until child creation")
			})
		}
	}
}

func TestBatchSpecStandaloneDepthDefaultAndValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		raw     string
		invalid bool
	}{
		{name: "default", raw: `{"items":[]}`},
		{name: "negative", raw: `{"max_agent_depth":-1,"items":[]}`, invalid: true},
		{name: "over maximum", raw: `{"max_agent_depth":33,"items":[]}`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			cmd.Flags().String("spec", "-", "")
			cmd.SetIn(strings.NewReader(tc.raw))
			_, depth, err := parseBatchSpecWithDepth(cmd)
			if tc.invalid {
				require.ErrorContains(t, err, "between 0 and 32")
				return
			}
			require.NoError(t, err)
			require.Equal(t, agent.DefaultAgentDepth, depth)
		})
	}
}
