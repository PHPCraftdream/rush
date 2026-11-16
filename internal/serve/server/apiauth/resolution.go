package apiauth

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
)

// Listener route values: "" and RouteFull are the same unrestricted default.
const (
	RouteFull  = "full"
	RouteBatch = "batch"
)

// batchOnlyOperations are the batch-intake routes a batch-only credential
// reaches. They are denied to session pins (see restSessionResolution) and
// allowed here.
var batchOnlyOperations = map[string]bool{
	"createBatch": true, "createTask": true, "getBatch": true, "getBatchNode": true,
	"stopBatch": true, "stopBatchNode": true, "answerBatchQuestion": true,
}

// resolveBatchOnlyRoute is the REST rule for a batch-only principal (a token
// with a batch binding, or any token on a listener with route "batch"): batch
// intake plus reads, run and permission handling the token owns, cancellation
// and held-question answers. Session runs, injects, creation, forks and
// deletes are refused, and so is every route the table does not classify.
func ResolveBatchOnlyRoute(operationID string) int {
	if batchOnlyOperations[operationID] {
		return 0
	}
	rule, ok := restSessionResolution[operationID]
	if !ok {
		return http.StatusForbidden
	}
	switch rule.class {
	case "read", "owned", "filtered":
		return 0
	}
	return http.StatusForbidden
}

// resolveBatchOnlyCommand is the WebSocket twin: read commands and agent
// cancellation only. Unknown commands are refused.
func ResolveBatchOnlyCommand(command string) int {
	if command == wire.CmdCancelAgent {
		return 0
	}
	rule, ok := sessionResolutionTable[command]
	if ok && rule.class == "read" {
		return 0
	}
	return http.StatusForbidden
}

type sessionResolution struct {
	field string
	class string
}

// sessionResolutionTable is exhaustive over inbound protocol commands. An
// empty field denotes an operation which has no session target; these are
// still classified so pinned credentials cannot invoke global mutations.
var sessionResolutionTable = map[string]sessionResolution{
	wire.CmdSendMessage: {"sessionID", "work"}, wire.CmdInterruptAndSend: {"sessionID", "work"},
	wire.CmdInjectMessage: {"sessionID", "work"}, wire.CmdCancelAgent: {"sessionID", "work"},
	wire.CmdCreateSession: {"", "deny"}, wire.CmdForkSession: {"sessionID", "deny"},
	wire.CmdDeleteSession: {"sessionID", "deny"}, wire.CmdDeleteOtherSessions: {"", "deny"},
	wire.CmdListSessions: {"", "read"}, wire.CmdLoadMessages: {"sessionID", "read"},
	wire.CmdGetConfig: {"", "deny"}, wire.CmdGetLogs: {"", "deny"}, wire.CmdSetTheme: {"", "deny"},
	wire.CmdSetKeepAlive: {"", "deny"}, wire.CmdRenameSession: {"sessionID", "work"},
	wire.CmdSetSessionModels: {"sessionID", "deny"}, wire.CmdRemoveRecentModel: {"sessionID", "deny"},
	wire.CmdTrackModelUsage: {"sessionID", "deny"}, wire.CmdDeleteMessage: {"sessionID", "work"},
	wire.CmdDeleteMessages: {"sessionID", "work"}, wire.CmdUpdateMessageContent: {"sessionID", "work"},
	wire.CmdUpdateMessageThinking: {"sessionID", "work"}, wire.CmdGetSystemPrompt: {"", "deny"},
	wire.CmdSetSystemPrompt: {"", "deny"}, wire.CmdSummarizeSession: {"sessionID", "work"},
	wire.CmdCancelQueuedSummarize: {"sessionID", "work"}, wire.CmdDeleteMessagePart: {"sessionID", "work"},
	wire.CmdUpdateMessagePart: {"sessionID", "work"}, wire.CmdTogglePinMessage: {"sessionID", "work"},
	wire.CmdRerunMessage: {"sessionID", "work"}, wire.CmdGetScopedModels: {"", "deny"},
	wire.CmdSetScopedModel: {"", "deny"}, wire.CmdClearScopedModel: {"", "deny"},
	wire.CmdGetCustomRoles: {"", "deny"}, wire.CmdSetCustomRole: {"", "deny"}, wire.CmdRemoveCustomRole: {"", "deny"},
	wire.CmdGetSessionLiveWork: {"sessionID", "read"}, wire.CmdGetSessionWakeSchedules: {"sessionID", "read"},
	wire.CmdGetSessionBatchWork: {"sessionID", "read"}, wire.CmdBatchStatus: {"sessionID", "read"},
	wire.CmdBatchOutput: {"sessionID", "read"}, wire.CmdBatchStop: {"sessionID", "deny"},
	wire.CmdBatchAdd: {"sessionID", "deny"}, wire.CmdBatchDrain: {"sessionID", "deny"},
	wire.CmdCancelWakeSchedule: {"sessionID", "work"}, wire.CmdShutdownServer: {"", "deny"},
	"set_debug": {"", "deny"}, "remove_context_path": {"", "deny"},
	"get_skills": {"", "deny"}, "add_skills_path": {"", "deny"},
	"remove_skills_path": {"", "deny"}, "remove_custom_provider": {"", "deny"},
	"set_provider_peak_hours": {"", "deny"}, "update_todos": {"sessionID", "work"},
	wire.CmdPauseProviderPeakHours: {"", "deny"}, wire.CmdResumeProviderPeakHours: {"", "deny"},
	wire.CmdGetPeakHoursStatus: {"", "deny"},
	"log_client_event":         {"", "deny"}, "log_client_error": {"", "deny"},
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
func ResolvePinnedCommand(pin, command string, raw json.RawMessage) int {
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
func ResolvePinnedRoute(pin, operationID, pathID string) int {
	if pin == "" {
		return 0
	}
	rule, ok := restSessionResolution[operationID]
	if !ok {
		return http.StatusForbidden
	}
	if rule.field == "id" && pathID != pin {
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

func RouteClassIsKnown(operationID string) bool {
	_, ok := restSessionResolution[operationID]
	return ok && strings.TrimSpace(operationID) != ""
}

// SessionResolutionCommands is the designed port (S2, lane L2): the keys of
// the session-resolution table for cross-package tests (api_batch_only_test.go
// stays in package server; sessionResolutionCommands was unused at HEAD).
func SessionResolutionCommands() []string {
	commands := make([]string, 0, len(sessionResolutionTable))
	for command := range sessionResolutionTable {
		commands = append(commands, command)
	}
	return commands
}

// SessionResolutionFor is the designed port (S2, lane L2): a read-only
// accessor over the session-resolution table for cross-package tests
// (handlers_custom_roles_test.go:117 stays in package server).
func SessionResolutionFor(command string) (field, class string, ok bool) {
	rule, ok := sessionResolutionTable[command]
	return rule.field, rule.class, ok
}
