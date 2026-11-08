package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

type sessionResolution struct {
	field string
	class string
}

// sessionResolutionTable is exhaustive over inbound protocol commands. An
// empty field denotes an operation which has no session target; these are
// still classified so pinned credentials cannot invoke global mutations.
var sessionResolutionTable = map[string]sessionResolution{
	CmdSendMessage: {"sessionID", "work"}, CmdInterruptAndSend: {"sessionID", "work"},
	CmdInjectMessage: {"sessionID", "work"}, CmdCancelAgent: {"sessionID", "work"},
	CmdCreateSession: {"", "deny"}, CmdForkSession: {"sessionID", "deny"},
	CmdDeleteSession: {"sessionID", "deny"}, CmdDeleteOtherSessions: {"", "deny"},
	CmdListSessions: {"", "read"}, CmdLoadMessages: {"sessionID", "read"},
	CmdGetConfig: {"", "deny"}, CmdGetLogs: {"", "deny"}, CmdSetTheme: {"", "deny"},
	CmdSetKeepAlive: {"", "deny"}, CmdRenameSession: {"sessionID", "work"},
	CmdSetSessionModels: {"sessionID", "deny"}, CmdRemoveRecentModel: {"sessionID", "deny"},
	CmdTrackModelUsage: {"sessionID", "deny"}, CmdDeleteMessage: {"sessionID", "work"},
	CmdDeleteMessages: {"sessionID", "work"}, CmdUpdateMessageContent: {"sessionID", "work"},
	CmdUpdateMessageThinking: {"sessionID", "work"}, CmdGetSystemPrompt: {"", "deny"},
	CmdSetSystemPrompt: {"", "deny"}, CmdSummarizeSession: {"sessionID", "work"},
	CmdCancelQueuedSummarize: {"sessionID", "work"}, CmdDeleteMessagePart: {"sessionID", "work"},
	CmdUpdateMessagePart: {"sessionID", "work"}, CmdTogglePinMessage: {"sessionID", "work"},
	CmdRerunMessage: {"sessionID", "work"}, CmdGetScopedModels: {"", "deny"},
	CmdSetScopedModel: {"", "deny"}, CmdClearScopedModel: {"", "deny"},
	CmdGetSessionLiveWork: {"sessionID", "read"}, CmdGetSessionWakeSchedules: {"sessionID", "read"},
	CmdGetSessionBatchWork: {"sessionID", "read"}, CmdBatchStatus: {"sessionID", "read"},
	CmdBatchOutput: {"sessionID", "read"}, CmdBatchStop: {"sessionID", "deny"},
	CmdBatchAdd: {"sessionID", "deny"}, CmdBatchDrain: {"sessionID", "deny"},
	CmdCancelWakeSchedule: {"sessionID", "work"}, CmdShutdownServer: {"", "deny"},
	"set_debug": {"", "deny"}, "remove_context_path": {"", "deny"},
	"get_skills": {"", "deny"}, "add_skills_path": {"", "deny"},
	"remove_skills_path": {"", "deny"}, "remove_custom_provider": {"", "deny"},
	"set_provider_peak_hours": {"", "deny"}, "update_todos": {"sessionID", "work"},
	CmdPauseProviderPeakHours: {"", "deny"}, CmdResumeProviderPeakHours: {"", "deny"},
	CmdGetPeakHoursStatus: {"", "deny"},
	"log_client_event":    {"", "deny"}, "log_client_error": {"", "deny"},
	"set_mcp_disabled": {"", "deny"}, "remove_mcp_server": {"", "deny"},
	"set_lsp_disabled": {"", "deny"}, "add_lsp_server": {"", "deny"},
	"remove_lsp_server": {"", "deny"}, "update_lsp_server": {"", "deny"},
	"set_provider_key": {"", "deny"}, "remove_provider_key": {"", "deny"},
	"add_mcp_server": {"", "deny"}, "update_mcp_server": {"", "deny"},
	"add_context_path": {"", "deny"}, "initialize_project": {"", "deny"},
	"add_custom_provider": {"", "deny"}, "update_custom_provider": {"", "deny"},
}

// resolvePinnedCommand performs visibility before operation-class enforcement.
// Missing indirect ownership data is intentionally treated as invisible.
func resolvePinnedCommand(pin, command string, raw json.RawMessage) int {
	if pin == "" {
		return 0
	}
	rule, ok := sessionResolutionTable[command]
	if !ok {
		return http.StatusForbidden
	}
	if rule.field != "" {
		var payload map[string]json.RawMessage
		if json.Unmarshal(raw, &payload) != nil {
			return http.StatusNotFound
		}
		var target string
		_ = json.Unmarshal(payload[rule.field], &target)
		if target != pin {
			return http.StatusNotFound
		}
	}
	if rule.class == "deny" {
		return http.StatusForbidden
	}
	return 0
}

var restSessionResolution = map[string]sessionResolution{
	"getSession": {"id", "read"}, "listMessages": {"id", "read"},
	"createRun": {"id", "work"}, "injectMessage": {"id", "work"},
	"getRun": {"", "owned"}, "cancelRun": {"", "owned"},
	"createSession": {"", "deny"}, "forkSession": {"id", "deny"},
	"deleteSession": {"id", "deny"}, "listSessions": {"", "filtered"},
	"getHealth": {"", "read"}, "getVersion": {"", "read"},
	"getOpenAPI": {"", "read"}, "listModels": {"", "read"},
	"listPermissions": {"", "owned"}, "answerPermission": {"", "owned"},
	"streamSessionEvents": {"id", "read"},
	// Batch intake belongs to unpinned and batch-bound tokens: a session pin never reaches it.
	"createBatch": {"", "deny"}, "createTask": {"", "deny"}, "getBatch": {"", "deny"},
	"getBatchNode": {"", "deny"}, "stopBatch": {"", "deny"}, "stopBatchNode": {"", "deny"}, "answerBatchQuestion": {"", "deny"},
}

// resolvePinnedRoute applies the same fail-closed visibility/class ordering to
// REST handlers before their implementation is entered. Two route classes are
// resolved INSIDE the handlers because the session is not in the request:
// "owned" (runs and permission requests are looked up by the creating token id,
// so a pinned token only ever finds the runs it started on its own session and
// a foreign id is the same 404 as a missing one) and "filtered" (listSessions
// returns only the pinned session).
func resolvePinnedRoute(pin string, route APIRoute, r *http.Request) int {
	if pin == "" {
		return 0
	}
	rule, ok := restSessionResolution[route.OperationID]
	if !ok {
		return http.StatusForbidden
	}
	if rule.field == "id" && r.PathValue("id") != pin {
		return http.StatusNotFound
	}
	if rule.class == "deny" {
		return http.StatusForbidden
	}
	return 0
}

func sessionResolutionCommands() []string {
	commands := make([]string, 0, len(sessionResolutionTable))
	for command := range sessionResolutionTable {
		commands = append(commands, command)
	}
	return commands
}

func routeClassIsKnown(route APIRoute) bool {
	_, ok := restSessionResolution[route.OperationID]
	return ok && strings.TrimSpace(route.OperationID) != ""
}
