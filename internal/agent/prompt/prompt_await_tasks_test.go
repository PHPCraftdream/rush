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
	require.NotContains(t, without, awaitTasksRuleLine,
		"without a worker the whole orchestrator block (the rule with it) is absent")
	require.NotContains(t, without, "Orchestrator mode",
		"without a worker the whole orchestrator block (the rule with it) is absent")
}

func TestBuild_OrchestratorDelegationContract(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	cfg.Models[config.SelectedModelTypeWorker] = registerProvider(cfg, "worker-provider", "worker-model", 200_000)
	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), true)
	require.NoError(t, err)
	without, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), false)
	require.NoError(t, err)

	for _, instruction := range []string{
		"Delegation contract:",
		"A worker ending without a final report, or without concrete diff/evidence for an edit task, has FAILED.",
		"Inspect worktree `git status` and `git diff`, then re-delegate once with a narrower, fully specified task.",
		"Never say done or end without a deliverable on an edit task.",
		"Answer a worker's `ask_question` immediately and conservatively by resuming it with `resume_session_id`; never wait for a human.",
		"While a worker runs, wait only for its completion via `await_tasks`, never a wake schedule or unrelated timer.",
		"Never repeat an already-run verification command without changed input; independent verification of worker claims remains required.",
		"The final reply must be the work report: files changed, commands and real output, and what could not be done, never a background-notice acknowledgement.",
		"A worker's command claim is unverified until visible in evidence such as diff or command output; report which claims were verified and which could not be verified.",
	} {
		require.Contains(t, got, instruction)
		require.NotContains(t, without, instruction,
			"each delegation-contract line must be absent when WorkerAvailable is false")
	}
}

// TestBuild_WorkerWaitRule_NamesNativePollTool pins the unconditional
// batch-world coder.md.tpl wait rule: the orchestrator wait rule names the
// native poll tool (batch_output) and the legacy job_output name never
// renders.
func TestBuild_WorkerWaitRule_NamesNativePollTool(t *testing.T) {
	store := testConfigStore(t)
	cfg := store.Config()
	cfg.Models[config.SelectedModelTypeWorker] = registerProvider(cfg, "worker-provider", "worker-model", 200_000)

	p := newTestCoderPrompt(t, store.WorkingDir())

	got, err := p.Build(context.Background(), "smart-provider", "smart-model", store, store.Config(), true)
	require.NoError(t, err)
	require.Contains(t, got, "never repeated `inspect_agent`/`batch_output` polling",
		"with the batch runtime on the orchestrator wait rule must name the native poll tool")
	require.NotContains(t, got, "`job_output` polling",
		"the legacy poll tool name must not render while the batch runtime is on")
}
