package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func intakeTask(t *testing.T, tool string) jobbatch.Spec {
	t.Helper()
	payload, err := json.Marshal(agent.BatchCLITask{Tool: tool, Input: json.RawMessage(`{"prompt":"p"}`)})
	require.NoError(t, err)
	return jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: payload}}
}

func intakeBatch(items ...jobbatch.Spec) jobbatch.Spec {
	return jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel, Notify: jobbatch.NotifyAll, Items: items}}
}

func TestBatchIntake_AgentOnlyRejectsNestedRunCommandBeforeConstruction(t *testing.T) {
	policy := batchIntakePolicy{}
	nested := intakeBatch(intakeTask(t, "agent"), intakeBatch(intakeTask(t, "run_command")))
	_, err := policy.normalize(nested)
	require.ErrorContains(t, err, `"run_command" is not allowed`)

	_, err = policy.normalize(intakeBatch(intakeTask(t, "agent"), intakeTask(t, "bash")))
	require.ErrorContains(t, err, `"bash" is not allowed`)
	_, err = policy.normalize(intakeTask(t, "agentic_fetch"))
	require.ErrorContains(t, err, `"agentic_fetch" is not allowed`, "agentic_fetch is off unless listed by exact name")

	_, err = policy.normalize(intakeBatch(intakeTask(t, "agent"), intakeBatch(intakeTask(t, "agent"))))
	require.NoError(t, err)
}

func TestBatchIntakeExactNameOptIn(t *testing.T) {
	policy := batchIntakePolicy{Tools: []string{"bash", "mcp_files_read", "agentic_fetch"}}
	for _, tool := range []string{"agent", "bash", "mcp_files_read", "agentic_fetch"} {
		_, err := policy.normalize(intakeTask(t, tool))
		require.NoError(t, err, tool)
	}
	for _, tool := range []string{"run_command", "mcp_files_write", "mcp_files", "bas", "BASH", ""} {
		_, err := policy.normalize(intakeTask(t, tool))
		require.Error(t, err, "%q is not listed by exact name", tool)
	}
}

func TestBatchIntakeReadOnlyTokenGetsOnlyReadOnlyTools(t *testing.T) {
	policy := batchIntakePolicy{ReadOnly: true, Tools: []string{"bash", "agent", "agentic_fetch", "mcp_files_read", "view"}}
	for _, tool := range []string{"view", "grep", "glob", "ls"} {
		_, err := policy.normalize(intakeTask(t, tool))
		require.NoError(t, err, tool)
	}
	for _, tool := range []string{"agent", "agentic_fetch", "mcp_files_read", "bash", "fs_write"} {
		_, err := policy.normalize(intakeTask(t, tool))
		require.Error(t, err, "%s must be refused for a read-only token even when listed", tool)
	}
}

func TestBatchIntakeForcesParallelismAndTimeouts(t *testing.T) {
	policy := batchIntakePolicy{MaxParallel: 2, MaxItemTimeout: time.Hour}
	spec := intakeBatch(intakeTask(t, "agent"), intakeBatch(intakeTask(t, "agent")))
	spec.Batch.MaxParallel = 50
	spec.Batch.Items[1].Batch.MaxParallel = 0
	out, err := policy.normalize(spec)
	require.NoError(t, err)
	require.Equal(t, 2, out.Batch.MaxParallel, "a larger cap is lowered to the token cap")
	require.Equal(t, 2, out.Batch.Items[1].Batch.MaxParallel, "an absent cap is forced")
	require.Equal(t, time.Hour, out.Batch.Items[0].Task.Timeout, "an absent item timeout becomes the token maximum")
	require.Equal(t, time.Duration(0), spec.Batch.Items[0].Task.Timeout, "the caller's spec is not mutated")

	sequential := intakeBatch(intakeTask(t, "agent"))
	sequential.Batch.Mode = jobbatch.ModeSequential
	sequential.Batch.MaxParallel = 9
	out, err = policy.normalize(sequential)
	require.NoError(t, err)
	require.Equal(t, 1, out.Batch.MaxParallel, "a sequential batch is capped at one, as the engine requires")

	explicit := intakeTask(t, "agent")
	explicit.Task.Timeout = 10 * time.Minute
	out, err = policy.normalize(explicit)
	require.NoError(t, err)
	require.Equal(t, 10*time.Minute, out.Task.Timeout)

	explicit.Task.Timeout = 2 * time.Hour
	_, err = policy.normalize(explicit)
	require.ErrorContains(t, err, "exceeds the token maximum")
	explicit.Task.Timeout = -time.Second
	_, err = policy.normalize(explicit)
	require.Error(t, err)

	out, err = (batchIntakePolicy{}).normalize(intakeTask(t, "agent"))
	require.NoError(t, err)
	require.Equal(t, batchIntakeDefaultTimeout, out.Task.Timeout, "the default timeout is 30 minutes")
}

func TestBatchIntakeNodeAndDepthLimits(t *testing.T) {
	policy := batchIntakePolicy{}
	items := make([]jobbatch.Spec, 0, batchIntakeMaxNodes)
	for i := 0; i < batchIntakeMaxNodes-1; i++ {
		items = append(items, intakeTask(t, "agent"))
	}
	_, err := policy.normalize(intakeBatch(items...))
	require.NoError(t, err, "root + 63 leaves is exactly 64 nodes")
	_, err = policy.normalize(intakeBatch(append(items, intakeTask(t, "agent"))...))
	require.ErrorContains(t, err, "nodes")

	deep := intakeBatch(intakeBatch(intakeTask(t, "agent")))
	_, err = policy.normalize(deep)
	require.NoError(t, err, "three levels are allowed")
	_, err = policy.normalize(intakeBatch(deep))
	require.ErrorContains(t, err, "levels deep")

	// A caller-side policy cannot raise the ceilings.
	_, err = (batchIntakePolicy{MaxNodes: 1000, MaxDepth: 50}).normalize(intakeBatch(deep))
	require.Error(t, err)
}

func TestBatchIntakeRejectsMalformedItems(t *testing.T) {
	policy := batchIntakePolicy{}
	_, err := policy.normalize(jobbatch.Spec{})
	require.Error(t, err, "an empty spec")
	both := intakeTask(t, "agent")
	both.Batch = &jobbatch.BatchSpec{Mode: jobbatch.ModeParallel}
	_, err = policy.normalize(both)
	require.Error(t, err, "task and batch together")
	_, err = policy.normalize(jobbatch.Spec{Task: &jobbatch.TaskSpec{Payload: json.RawMessage(`not json`)}})
	require.Error(t, err, "an unparsable payload")
}
