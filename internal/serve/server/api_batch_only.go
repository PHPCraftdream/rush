package server

import "net/http"

// Listener route values: "" and routeFull are the same unrestricted default.
const (
	routeFull  = "full"
	routeBatch = "batch"
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
func resolveBatchOnlyRoute(route APIRoute) int {
	if batchOnlyOperations[route.OperationID] {
		return 0
	}
	rule, ok := restSessionResolution[route.OperationID]
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
func resolveBatchOnlyCommand(command string) int {
	if command == CmdCancelAgent {
		return 0
	}
	rule, ok := sessionResolutionTable[command]
	if ok && rule.class == "read" {
		return 0
	}
	return http.StatusForbidden
}
