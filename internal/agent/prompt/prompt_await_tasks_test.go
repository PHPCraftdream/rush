package prompt

// The await_tasks rule (#1270) in coder.md.tpl's orchestrator block: it must
// render as its own paragraph when a worker is available, and be absent with
// the rest of the block when it is not.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// awaitTasksRuleLine is the exact first line of the rule's own paragraph in
// coder.md.tpl. Keying the test on the paragraph START (with the leading
// newline) is what makes the "own paragraph, not glued onto rule 7" shape
// testable: a revert that re-joins the sentence onto rule 7's line fails the
// Contains even though the words themselves are still there.
const awaitTasksRuleLine = "\nWhile workers or your own jobs are running and you have nothing else to do, call `await_tasks`"

// TestBuild_AwaitTasksRule_TracksWorkerAvailable checks the rule renders with
// the orchestrator block and disappears with it.
func TestBuild_AwaitTasksRule_TracksWorkerAvailable(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	cfg.Models[config.SelectedModelTypeWorker] = registerProvider(cfg, "worker-provider", "worker-model", 200_000)

	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), true)
	require.NoError(t, err)

	require.Contains(t, got, awaitTasksRuleLine,
		"the await_tasks rule must render as its own paragraph when a worker is available")
	require.Contains(t, got, "never `ask_question`",
		"the rule must forbid ask_question as a wait")
	require.Contains(t, got, "until: \"all\"",
		"the rule must mention the until-all mode")
	require.Contains(t, got, "with `inject_agent`.\nWhile workers or your own jobs are running",
		"the rule must start on its own line, not appended to rule 7's paragraph")

	without, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), false)
	require.NoError(t, err)
	require.NotContains(t, without, "await_tasks",
		"without a worker the whole orchestrator block (the rule with it) is absent")
}
