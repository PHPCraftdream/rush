package agent

import (
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
)

func batchControlAvailability(filteredTools []fantasy.AgentTool, allowed []string) []string {
	return batchinst.ControlAvailability(filteredTools, allowed, batchWorkTarget)
}

func workerToolNamesBatch(names []string) []string { return batchinst.WorkerToolNames(names) }

func workerLayeredToolNames() []string { return workerToolNamesBatch(workerToolNames) }

func folderScopeCommandToolSet() map[string]struct{} { return batchinst.FolderScopeCommandToolSet() }
