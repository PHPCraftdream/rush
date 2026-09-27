package agent

// Regression test for task #1050: nested-delegation recursion guard.
//
// buildTools' `slices.Contains(agent.AllowedTools, AgentToolName)` branch
// used to call c.agentTool(ctx) unconditionally. If config.AgentTask's
// AllowedTools ever contained "agent" (AgentToolName), c.agentTool would
// build the task agent again via c.buildAgent, whose own buildTools call
// would see AgentToolName in its AllowedTools too and recurse forever --
// each level registering two more goroutines on the shared c.readyWg that
// every run entry point blocks on via readyWg.Wait(). This is unreachable
// today (SetupAgents gives AgentTask a fixed read-only list, and
// UpdateAgentAllowedTools is only ever called for the coder agent), but
// the guard in coordinator_tools.go must hold no matter how AllowedTools
// picked up the entry -- config edit, future code, etc.
//
// This test forces exactly that shape: it grants the task agent config a
// self-referential AllowedTools (including "agent") and asserts
// buildAgent/readyWg.Wait() still completes promptly and the resulting
// tool list never contains the agent tool.

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/prompt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestBuildTools_TaskAgentNeverGetsAgentTool(t *testing.T) {
	env := testEnv(t)
	coord := newToolPinningCoordinator(t, env, false)

	taskCfg, ok := coord.cfg.Config().Agents[config.AgentTask]
	require.True(t, ok, "task agent must be configured")

	// Simulate a config that (incorrectly) grants the task agent the
	// "agent" tool -- the exact shape the guard must reject, regardless of
	// how AllowedTools got there.
	allowed := make([]string, len(taskCfg.AllowedTools), len(taskCfg.AllowedTools)+1)
	copy(allowed, taskCfg.AllowedTools)
	allowed = append(allowed, AgentToolName)
	taskCfg.AllowedTools = allowed

	tp, err := taskPrompt(prompt.WithWorkingDir(env.workingDir))
	require.NoError(t, err)

	type buildResult struct {
		agent SessionAgent
		err   error
	}
	done := make(chan buildResult, 1)
	go func() {
		subAgent, buildErr := coord.buildAgent(context.Background(), tp, taskCfg, true)
		if buildErr != nil {
			done <- buildResult{err: buildErr}
			return
		}
		done <- buildResult{agent: subAgent, err: coord.readyWg.Wait()}
	}()

	var result buildResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("buildAgent/readyWg.Wait() did not return within 5s -- nested agent-tool recursion (task #1050) is back")
	}
	require.NoError(t, result.err, "a self-referential task-agent AllowedTools must not fail the build")

	sa, ok := result.agent.(*sessionAgent)
	require.True(t, ok, "buildAgent must return a *sessionAgent")

	for _, tl := range sa.tools.Copy() {
		require.NotEqual(t, AgentToolName, tl.Info().Name,
			"the task agent must never receive the agent tool, regardless of AllowedTools")
	}
}
