package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestConcurrentRequestedRolesDoNotCrossContaminate(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	policy := &CallOptions{ModelRole: config.SelectedModelTypeSmart, DisableSubAgents: true, MaxAgentDepth: 2, AgentDepthSet: true}
	ctx = WithCallOptions(ctx, policy)
	type observation struct {
		role  RequestedModelRole
		model string
		depth int
	}
	observed := make(chan observation, 4)
	child := newMockAgent("role-fixture", 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		observed <- observation{role: call.RequestedModelRole, model: call.SmartModel.ModelCfg.Model, depth: call.CallOptions.MaxAgentDepth}
		return agentResultWithText("concurrent role result"), nil
	})
	errors := make(chan error, 4)
	var workers sync.WaitGroup
	for _, role := range []RequestedModelRole{"smart", "fast", "worker", "reviewer"} {
		workers.Add(1)
		go func(role RequestedModelRole) {
			defer workers.Done()
			response, err := coord.runSubAgent(ctx, subAgentParams{
				Agent: child, SessionID: parent, AgentMessageID: "concurrent-role", ToolCallID: string(role),
				Prompt: "p", SessionTitle: "role", RequestedModelRole: role, slotModel: true,
			})
			if err == nil && response.IsError {
				err = fmt.Errorf("role %s delegation was refused", role)
			}
			errors <- err
		}(role)
	}
	workers.Wait()
	close(errors)
	close(observed)
	for err := range errors {
		require.NoError(t, err)
	}
	seen := map[RequestedModelRole]bool{}
	for got := range observed {
		require.Equal(t, string(got.role)+"-model", got.model)
		require.Equal(t, 1, got.depth)
		seen[got.role] = true
	}
	require.Len(t, seen, 4)
	require.Equal(t, config.SelectedModelTypeSmart, policy.ModelRole)
	require.True(t, policy.DisableSubAgents)
	require.Equal(t, 2, policy.MaxAgentDepth)
}

func TestRequestedRoleRefreshesInferenceSettingsWithoutChangingTheRole(t *testing.T) {
	coord, parent := requestedRoleCoordinator(t)
	var observed []SessionAgentCall
	child := newMockAgent("role-fixture", 128, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		observed = append(observed, call)
		return agentResultWithText("live role result"), nil
	})
	params := subAgentParams{Agent: child, SessionID: parent, AgentMessageID: "m", ToolCallID: "initial", Prompt: "p", SessionTitle: "live role", RequestedModelRole: "worker", slotModel: true}
	_, err := coord.runSubAgent(t.Context(), params)
	require.NoError(t, err)
	profile := coord.cfg.Config().Models[config.SelectedModelTypeWorker]
	profile.MaxTokens = 222
	coord.cfg.Config().Models[config.SelectedModelTypeWorker] = profile
	params.ResumeSessionID = observed[0].SessionID
	params.RequestedModelRole = ""
	_, err = coord.runSubAgent(t.Context(), params)
	require.NoError(t, err)
	require.Len(t, observed, 2)
	require.Equal(t, observed[0].SessionID, observed[1].SessionID)
	require.Equal(t, RequestedModelRole("worker"), observed[1].RequestedModelRole)
	require.Equal(t, "worker-model", observed[1].SmartModel.ModelCfg.Model)
	require.EqualValues(t, 102, observed[0].MaxOutputTokens)
	require.EqualValues(t, 222, observed[1].MaxOutputTokens)
}
