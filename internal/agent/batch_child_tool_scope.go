package agent

import (
	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
)

// pctCapturedAgentToolFromTool narrows the policy walk to the concrete child wrapper.
func pctCapturedAgentToolFromTool(tool fantasy.AgentTool) *childagent.PctCapturedAgentTool {
	captured, _ := toolwrap.CapturedToolFrom(tool).(*childagent.PctCapturedAgentTool)
	return captured
}
