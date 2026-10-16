package agent

import (
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
)

func batchFlagEnabled() bool { return config.BatchRuntimeEnabled() }

func batchControlAvailability(filteredTools []fantasy.AgentTool) []string {
	present := make(map[string]bool, len(filteredTools))
	for _, tool := range filteredTools {
		present[tool.Info().Name] = true
	}
	var names []string
	if present["await_tasks"] {
		names = append(names, "await_tasks")
	}
	work := false
	for name := range present {
		if batchWorkTarget(name) {
			work = true
			break
		}
	}
	if work {
		names = append(names, "batch_run", "batch_add")
	}
	if present["job_output"] {
		names = append(names, "batch_status", "batch_output")
	}
	if present["job_kill"] {
		names = append(names, "batch_stop")
	}
	return names
}
