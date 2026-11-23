package agent

import (
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
)

func batchControlAvailability(filteredTools []fantasy.AgentTool, allowed []string) []string {
	return batchinst.ControlAvailability(filteredTools, allowed, batchWorkTarget)
}

func workerLayeredToolNames() []string { return batchinst.WorkerToolNames(workerToolNames) }
