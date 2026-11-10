package agent

import (
	"encoding/json"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

func editMadeNoWrite(call fantasy.ToolCallContent, result fantasy.ToolResultContent) bool {
	if (call.ToolName != tools.EditToolName && call.ToolName != tools.MultiEditToolName) || result.Result == nil || result.Result.GetType() != fantasy.ToolResultContentTypeError {
		return false
	}
	var metadata tools.EditEffectMetadata
	if json.Unmarshal([]byte(result.ClientMetadata), &metadata) != nil || !metadata.EditEffect.NoWrite {
		return false
	}
	switch metadata.EditEffect.Reason {
	case "missing_old", "equal_content", "multiple_matches":
		return call.ToolName == tools.EditToolName
	case "unchanged_content":
		return call.ToolName == tools.MultiEditToolName
	default:
		return false
	}
}
