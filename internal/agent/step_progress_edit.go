package agent

import (
	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
)

func editMadeNoWrite(call fantasy.ToolCallContent, result fantasy.ToolResultContent) bool {
	return turnstep.EditMadeNoWrite(call, result)
}
