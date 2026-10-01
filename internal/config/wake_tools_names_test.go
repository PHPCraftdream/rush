// Stage-4c: the wake tools' registration contract — the coder (and the
// root session) get all five; the task agent (read-only set) gets none;
// the restricted-run dispatch table classifies list=read, create=write,
// cancel=delete.
package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWakeToolsRegisteredInAllToolNames(t *testing.T) {
	names := allToolNames()
	for _, want := range []string{"wakein", "wakeon", "loop", "wake_list", "wake_cancel"} {
		require.Contains(t, names, want, "the coder's default toolset must include %s", want)
	}
}

func TestWakeTools_CoderGetsThemTaskAgentDoesNot(t *testing.T) {
	cfg := &Config{Options: &Options{}}
	cfg.SetupAgents()
	coder := cfg.Agents[AgentCoder]
	task := cfg.Agents[AgentTask]
	for _, want := range []string{"wakein", "wakeon", "loop", "wake_list", "wake_cancel"} {
		require.Contains(t, coder.AllowedTools, want, "the coder schedules wakes")
		require.NotContains(t, task.AllowedTools, want,
			"the task agent is read-only and must never get the wake tools, including the read-only wake_list (operator decision)")
	}
}
