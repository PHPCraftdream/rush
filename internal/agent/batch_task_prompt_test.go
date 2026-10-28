package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestTaskPromptRendersBatchLifecycleGuidance(t *testing.T) {
	isolateAllGlobalConfigPaths(t)
	workingDir := t.TempDir()
	store, err := config.Init(workingDir, "", false)
	require.NoError(t, err)

	task, err := taskPrompt(
		prompt.WithPlatform("linux"),
		prompt.WithWorkingDir(workingDir),
	)
	require.NoError(t, err)
	got, err := task.Build(context.Background(), "test-provider", "test-model", store, store.Config(), false)
	require.NoError(t, err)

	require.Contains(t, got, "started; completions arrive as selected notices. Call await_tasks to wait for it.")
	require.Contains(t, got, "Use `await_tasks` to wait for completion notices; do not poll; never use `ask_question` to wait.")
	require.Contains(t, got, "Use `batch_status`/`batch_output`")
	require.Contains(t, got, "use `batch_stop` only for batches you own or that belong to descendants.")

	for _, name := range []string{"await_tasks", "batch_status", "batch_output", "batch_stop"} {
		require.Contains(t, workerLayeredToolNames(), name, "task-worker guidance must refer to a wired tool")
	}
	require.Equal(t, batchStartedResponse("prompt-regression-root").Content, batchStartedNoticeText("prompt-regression-root"),
		"durable started notices must match the tool response exactly")
}
