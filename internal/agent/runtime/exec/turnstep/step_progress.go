package turnstep

import (
	"encoding/json"
	"strings"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
)

// editMadeNoWrite reports whether an edit/multi_edit result carries the
// adapter's no-write tag: the tool ran, matched (or failed to match) its
// targets, and provably changed nothing.
func EditMadeNoWrite(call fantasy.ToolCallContent, result fantasy.ToolResultContent) bool {
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

// IsWaitOnlyCall reports whether call is a pure wait: a bash command whose
// every invocation is a literal sleep/echo/printf/true/: with no redirects,
// pipes or substitutions, or a run_command whose program is sleep/timeout.
// Whether that wait is an ASYNC launch (wait) or a blocking sync call (act)
// is the metadata's business, decided by the step classifier.
func IsWaitOnlyCall(call fantasy.ToolCallContent) bool {
	switch call.ToolName {
	case tools.BashToolName:
		var params struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return false
		}
		return shell.IsNoOpCommand(params.Command)
	case tools.RunCommandToolName:
		var params struct {
			Program string `json:"program"`
		}
		if json.Unmarshal([]byte(call.Input), &params) != nil {
			return false
		}
		return params.Program == "sleep" || params.Program == "timeout"
	default:
		return false
	}
}

// ProgressGuardMetadata is the ClientMetadata tag the in-turn guard stamps on
// a result it refused (step 2 of the plan writes it): the tool never ran, so
// the step neither progressed nor launched anything.
type ProgressGuardMetadata struct {
	ProgressGuard struct {
		Refused string `json:"refused"`
	} `json:"progress_guard"`
}

// GuardRefused reports whether result carries the guard's refusal tag.
func GuardRefused(result fantasy.ToolResultContent) bool {
	var guard ProgressGuardMetadata
	if json.Unmarshal([]byte(result.ClientMetadata), &guard) != nil {
		return false
	}
	return guard.ProgressGuard.Refused != ""
}

// AsyncCapMetadata is the JSON shape the async-cap refusal emits as its
// ClientMetadata tag.
type AsyncCapMetadata struct {
	AsyncCap struct {
		Running    int `json:"running"`
		Limit      int `json:"limit"`
		WaitTimers int `json:"wait_timers"`
	} `json:"async_cap"`
}

// AsyncCapRefused reports whether result carries the async-cap tag of
// the deleted async-tool cap branch: admission refused the launch, so the tool
// never ran and took no claim -- the tool's own refusal, not the guard's.
func AsyncCapRefused(result fantasy.ToolResultContent) bool {
	var meta AsyncCapMetadata
	if json.Unmarshal([]byte(result.ClientMetadata), &meta) != nil {
		return false
	}
	return meta.AsyncCap.Running > 0 || meta.AsyncCap.Limit > 0
}

// UnknownToolResultPrefix is fantasy's bare answer to a call whose tool name
// is not in the step's set: nothing executed, so the call is neutral.
const UnknownToolResultPrefix = "tool not found:"

// UnknownToolResult reports whether result is that bare unknown-tool error.
func UnknownToolResult(result fantasy.ToolResultContent) bool {
	if result.Result == nil ||
		result.Result.GetType() != fantasy.ToolResultContentTypeError {
		return false
	}
	errResult, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result)
	if !ok || errResult.Error == nil {
		return false
	}
	return strings.Contains(errResult.Error.Error(), UnknownToolResultPrefix)
}
