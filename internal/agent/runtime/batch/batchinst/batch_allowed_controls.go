package batchinst

import (
	"slices"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// ControlAvailability grants await by presence, run/add by admissible work,
// and read/stop controls by the final per-call allowance, in stable order.
func ControlAvailability(filteredTools []fantasy.AgentTool, allowed []string, isWorkTarget func(string) bool) []string {
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
		if isWorkTarget(name) {
			work = true
			break
		}
	}
	if work {
		names = append(names, "batch_run", "batch_add")
	}
	for _, name := range []string{"batch_status", "batch_output", "batch_stop"} {
		if slices.Contains(allowed, name) {
			names = append(names, name)
		}
	}
	return names
}

// WorkerToolNames appends native read and scoped-stop controls without duplicates.
func WorkerToolNames(names []string) []string {
	out := make([]string, 0, len(names)+3)
	out = append(out, names...)
	for _, name := range []string{"batch_status", "batch_output", "batch_stop"} {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

var folderScopeCommandToolsBatch = map[string]struct{}{
	tools.BashToolName: {}, tools.RunCommandToolName: {},
	"batch_status": {}, "batch_output": {}, "batch_stop": {},
}

func FolderScopeCommandToolSet() map[string]struct{} {
	return folderScopeCommandToolsBatch
}
