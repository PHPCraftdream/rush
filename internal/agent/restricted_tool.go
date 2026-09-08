package agent

import (
	"context"
	"encoding/json"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
)

var restrictedToolActions = map[string]string{
	"agent":                      "delegate",
	"agentic_fetch":              "fetch",
	"ask_question":               "ask",
	"download":                   "download",
	"edit":                       "write",
	"fetch":                      "fetch",
	"fs_delete":                  "delete",
	"fs_find":                    "list",
	"fs_grep":                    "read",
	"fs_list":                    "list",
	"fs_read":                    "read",
	"fs_replace":                 "write",
	"fs_write":                   "write",
	"fs_write_lines":             "write",
	"git_read":                   "read",
	"glob":                       "read",
	"grep":                       "read",
	"job_kill":                   "delete",
	"job_output":                 "read",
	"list_mcp_resources":         "list",
	"ls":                         "list",
	"multiedit":                  "write",
	"read_delegation_transcript": "read",
	"read_mcp_resource":          "read",
	"rush_info":                  "read",
	"rush_logs":                  "read",
	"sourcegraph":                "read",
	"todos":                      "write",
	"view":                       "read",
	"web_fetch":                  "read",
	"web_search":                 "read",
	"write":                      "write",
}

func restrictedRunAuthorizer(service permission.Service) permission.RestrictedRunAuthorizer {
	if service == nil {
		return nil
	}
	authorizer, _ := service.(permission.RestrictedRunAuthorizer)
	return authorizer
}

// restrictedTool is the final dispatch gate for a run-scoped allowlist. It
// covers tools that predate permission.Request; command tools retain their
// command-level matcher through the same gate.
type restrictedTool struct {
	fantasy.AgentTool
	authorizer permission.RestrictedRunAuthorizer
}

func wrapToolsWithRestrictedRun(tools []fantasy.AgentTool, authorizer permission.RestrictedRunAuthorizer) []fantasy.AgentTool {
	if authorizer == nil {
		return tools
	}
	out := make([]fantasy.AgentTool, len(tools))
	for i, tool := range tools {
		if restrictedRunWrapped(tool) {
			out[i] = tool
			continue
		}
		out[i] = restrictedTool{AgentTool: tool, authorizer: authorizer}
	}
	return out
}

type restrictedRunMarker interface{ restrictedRunWrapped() }

func restrictedRunWrapped(tool fantasy.AgentTool) bool {
	if hooked, ok := tool.(*hookedTool); ok {
		return restrictedRunWrapped(hooked.inner)
	}
	_, ok := tool.(restrictedRunMarker)
	return ok
}

func (t restrictedTool) restrictedRunWrapped() {}

func (t restrictedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if permission.HookApprovalGranted(ctx, call.ID) {
		return t.AgentTool.Run(ctx, call)
	}
	sessionID := tools.GetSessionFromContext(ctx)
	dispatchAuthorizer, hasDispatchAuthorizer := t.authorizer.(permission.RestrictedRunDispatchAuthorizer)
	if params, ok := restrictedCommandParams(call.Name, call.Input); ok {
		opts := permission.CreatePermissionRequest{
			SessionID:  sessionID,
			ToolCallID: call.ID,
			ToolName:   call.Name,
			Action:     "execute",
			Params:     params,
		}
		var restricted, allowed bool
		if hasDispatchAuthorizer {
			restricted, allowed = dispatchAuthorizer.AuthorizeRestrictedDispatch(ctx, opts)
		} else {
			restricted, allowed = t.authorizer.AuthorizeRestrictedRun(opts)
		}
		if restricted && !allowed {
			return tools.NewPermissionDeniedResponse(), nil
		}
		return t.AgentTool.Run(ctx, call)
	}

	toolAuthorizer, ok := t.authorizer.(permission.RestrictedRunToolAuthorizer)
	if !ok {
		return t.AgentTool.Run(ctx, call)
	}
	action := restrictedToolActions[call.Name]
	if action == "" {
		if provider, ok := t.AgentTool.(interface{ RestrictedRunAction() string }); ok {
			action = provider.RestrictedRunAction()
		}
	}
	opts := permission.CreatePermissionRequest{
		SessionID:  sessionID,
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Action:     action,
	}
	var restricted, allowed bool
	if hasDispatchAuthorizer {
		restricted, allowed = dispatchAuthorizer.AuthorizeRestrictedDispatch(ctx, opts)
	} else {
		restricted, allowed = toolAuthorizer.AuthorizeRestrictedTool(opts)
	}
	if restricted && !allowed {
		return tools.NewPermissionDeniedResponse(), nil
	}
	return t.AgentTool.Run(ctx, call)
}

func restrictedCommandParams(toolName, input string) (any, bool) {
	switch toolName {
	case tools.BashToolName:
		var params tools.BashPermissionsParams
		if json.Unmarshal([]byte(input), &params) != nil {
			return nil, true
		}
		return params, true
	case tools.RunCommandToolName:
		var params tools.RunCommandPermissionsParams
		if json.Unmarshal([]byte(input), &params) != nil {
			return nil, true
		}
		return params, true
	default:
		return nil, false
	}
}
