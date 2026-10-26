package agent

import (
	"slices"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// batchControlAvailability reports, in stable order, the model-facing batch
// controls one assembled toolset offers. R12-H1: every grant is decided by a
// NATIVE fact of THIS build -- never by the legacy job_kill/job_output names
// (H4 deletes those tools, and a native control that still resolved through
// an old name would vanish with them):
//
//   - await_tasks: the constructed await tool is replaced in place by the
//     batch variant (tools.NewBatchAwaitTasksTool), so it still gates on its
//     own constructed presence;
//   - batch_run, batch_add: any batch-admissible work leaf present --
//     explicit batches of work the pinned role can already run directly;
//   - batch_status, batch_output, batch_stop: granted BY NAME from allowed,
//     the build's final AllowedTools after every per-call filter. Worker stop
//     scope is additionally pinned into its per-call control and enforced by
//     Stop.
func batchControlAvailability(filteredTools []fantasy.AgentTool, allowed []string) []string {
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
	for _, name := range []string{"batch_status", "batch_output", "batch_stop"} {
		if slices.Contains(allowed, name) {
			names = append(names, name)
		}
	}
	return names
}

// workerToolNamesBatch is the worker layering: the worker ALWAYS receives the
// native batch_status/batch_output read controls -- appended to the supplied
// names, dedup-safe, independently of any legacy grant list -- so its
// post-await read surface never depends on the caller's list carrying them.
// batch_stop is granted to workers only because its per-call control carries
// a trusted descendant-only scope enforced by Stop.
func workerToolNamesBatch(names []string) []string {
	out := make([]string, 0, len(names)+3)
	out = append(out, names...)
	for _, name := range []string{"batch_status", "batch_output", "batch_stop"} {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// workerLayeredToolNames picks the worker layering: unconditional batch
// acceptance (the batch selection IS the surface).
func workerLayeredToolNames() []string {
	return workerToolNamesBatch(workerToolNames)
}

// folderScopeCommandToolsBatch is the scoped command-tool set: batch_status,
// batch_output and batch_stop are kept or stripped exactly like the command
// tools they replace.
var folderScopeCommandToolsBatch = map[string]struct{}{
	tools.BashToolName: {}, tools.RunCommandToolName: {},
	"batch_status": {}, "batch_output": {}, "batch_stop": {},
}

// folderScopeCommandToolSet is the scoped command-tool set: unconditional
// batch acceptance.
func folderScopeCommandToolSet() map[string]struct{} {
	return folderScopeCommandToolsBatch
}
