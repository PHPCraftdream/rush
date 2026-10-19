package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// TestForkedWorkerReviewerPinsSurviveDiskReload exercises the real transactional
// fork followed by disk adoption, then proves role resolution and agent dispatch
// still honor the copied literal pins and effort.
func TestForkedWorkerReviewerPinsSurviveDiskReload(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "disk-config")
	dataDir := filepath.Join(t.TempDir(), "disk-data")
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_GLOBAL_DATA", dataDir)
	env := testEnv(t)
	const provider = "pin-provider"
	coord := newTestCoordinator(t, env, provider, config.ProviderConfig{
		ID: provider, Type: "openai",
		Models: []catwalk.Model{
			{ID: "smart", Name: "Smart", DefaultMaxTokens: 3000},
			{ID: "worker-pin", Name: "Worker pin", DefaultMaxTokens: 3000},
			{ID: "reviewer-pin", Name: "Reviewer pin", DefaultMaxTokens: 3000},
			{ID: "worker-new", Name: "Worker new", DefaultMaxTokens: 3000},
			{ID: "reviewer-new", Name: "Reviewer new", DefaultMaxTokens: 3000},
		},
	})
	coord.cfg.SetupAgents()
	ctx := t.Context()
	parent, err := env.sessions.Create(ctx, "pin parent")
	require.NoError(t, err)
	require.NoError(t, env.sessions.UpdateWorkerReviewerModels(ctx, parent.ID,
		&session.ModelSlotUpdate{Provider: provider, Model: "worker-pin"},
		&session.ModelSlotUpdate{Provider: provider, Model: "reviewer-pin"}))
	require.NoError(t, env.sessions.UpdateWorkerReviewerReasoningEffort(ctx, parent.ID, "high", "low"))
	fork, _, err := env.sessions.ForkSessionTx(ctx, parent.ID, session.ForkOptions{NewID: "forked-pins"})
	require.NoError(t, err)
	require.Equal(t, "worker-pin", fork.WorkerModelID)
	require.Equal(t, "reviewer-pin", fork.ReviewerModelID)
	require.Equal(t, "high", fork.WorkerModelReasoningEffort)
	require.Equal(t, "low", fork.ReviewerModelReasoningEffort)

	configPath := filepath.Join(configDir, "rush.json")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	initial := map[string]any{
		"models": map[string]any{
			"smart":    map[string]any{"provider": provider, "model": "smart"},
			"worker":   map[string]any{"provider": provider, "model": "worker-pin"},
			"reviewer": map[string]any{"provider": provider, "model": "reviewer-pin"},
		},
		"providers": map[string]any{provider: map[string]any{
			"type": "openai", "api_key": "test-key", "models": []any{
				map[string]any{"id": "smart", "name": "Smart"},
				map[string]any{"id": "worker-pin", "name": "Worker pin"},
				map[string]any{"id": "reviewer-pin", "name": "Reviewer pin"},
				map[string]any{"id": "worker-new", "name": "Worker new"},
				map[string]any{"id": "reviewer-new", "name": "Reviewer new"},
			},
		}},
	}
	writeConfig := func(worker, reviewer string) {
		t.Helper()
		models := initial["models"].(map[string]any)
		models["worker"] = map[string]any{"provider": provider, "model": worker}
		models["reviewer"] = map[string]any{"provider": provider, "model": reviewer}
		data, marshalErr := json.Marshal(initial)
		require.NoError(t, marshalErr)
		require.NoError(t, os.WriteFile(configPath, data, 0o600))
	}
	writeConfig("worker-pin", "reviewer-pin")
	diskCfg, err := config.Load(env.workingDir, dataDir, false)
	require.NoError(t, err)
	diskCfg.CaptureStalenessSnapshot([]string{configPath})
	coord.cfg = diskCfg
	writeConfig("worker-new", "reviewer-new")
	require.NoError(t, diskCfg.ReloadFromDisk(ctx))
	require.Equal(t, "worker-new", diskCfg.Config().Models[config.SelectedModelTypeWorker].Model)
	require.Equal(t, "reviewer-new", diskCfg.Config().Models[config.SelectedModelTypeReviewer].Model)

	workerCtx := WithCallOptions(ctx, &CallOptions{ModelRole: config.SelectedModelTypeWorker})
	resolved, err := coord.resolveSessionModelsInternal(workerCtx, fork.ID, false)
	require.NoError(t, err)
	require.Equal(t, "worker-pin", resolved.smart.ModelCfg.Model)
	require.Equal(t, "high", resolved.smart.ModelCfg.ReasoningEffort)
	reviewerCtx := WithCallOptions(ctx, &CallOptions{ModelRole: config.SelectedModelTypeReviewer})
	resolved, err = coord.resolveSessionModelsInternal(reviewerCtx, fork.ID, false)
	require.NoError(t, err)
	require.Equal(t, "reviewer-pin", resolved.smart.ModelCfg.Model)
	require.Equal(t, "low", resolved.smart.ModelCfg.ReasoningEffort)

	var observed SessionAgentCall
	driver := newMockAgent(provider, 3000, func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		observed = call
		return agentResultWithText("worker pin honored"), nil
	})
	driver.model = Model{ModelCfg: config.SelectedModel{Provider: provider, Model: "smart"}}
	resp, err := coord.runSubAgent(workerCtx, subAgentParams{
		Agent: driver, SessionID: fork.ID, AgentMessageID: "fork-dispatch", ToolCallID: "fork-worker",
		Prompt: "worker", SessionTitle: "fork worker", slotModel: true,
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "worker-pin", observed.SmartModel.ModelCfg.Model)
}
