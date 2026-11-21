package state

import (
	"context"
	"errors"
	"fmt"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// ScopeResult classifies the confirmed closure. A stop
// acknowledgement alone is never a settlement: cancellation is reported only
// after the scope confirmed closure.
func ScopeResult(response fantasy.ToolResponse, declared jobbatch.Outcome, turnErr error, stopped bool) jobbatch.Result {
	r := jobbatch.Result{}
	switch {
	case stopped && (errors.Is(turnErr, context.Canceled) || errors.Is(turnErr, context.DeadlineExceeded)):
		r.State = jobbatch.StateCancelled
	case turnErr != nil:
		r.State, r.Code = jobbatch.StateFailed, "agent_run_error"
		r.Summary = fmt.Sprintf("%v", turnErr)
	case response.IsError:
		r.State, r.Code = jobbatch.StateFailed, "agent_error"
		r.Summary = response.Content
	default:
		r.State = jobbatch.StateCompleted
		r.Summary = response.Content
		// A non-reportable declaration is ignored, never turned into an empty
		// state.
		if declared.State.Reportable() {
			r.State, r.Code, r.Summary = declared.State, declared.Code, declared.Summary
		}
	}
	if r.Summary == "" {
		r.Summary = response.Content
	}
	r.Details = batchdispatch.Envelope(response)
	return r
}

// PSCRefreshTimeout bounds one child final-outcome refresh at closure.
const PSCRefreshTimeout = 5 * time.Second

// RefreshTurnResponse replaces the captured initial
// response with the child's refreshed final outcome at confirmed closure.
// A leaf-run infrastructure error (turnErr non-nil) is never refreshed —
// the run failed before a child outcome exists. No child selected (or no
// facts wired): nothing to refresh. A refreshed failed initial keeps
// failing: the facts' marker-aware refresh (refreshSubAgentCompletion)
// checks reaction-failure markers FIRST, so a permanently failed reaction
// settlement delivers its failure, never a stale success.
func RefreshTurnResponse[P, I any](facts BatchChildReactionFacts, req RunRequest[P, I], response fantasy.ToolResponse, turnErr error) (fantasy.ToolResponse, bool) {
	if facts == nil || req.ChildSessionID == "" || req.NoChildSelected || turnErr != nil {
		return response, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), PSCRefreshTimeout)
	defer cancel()
	refreshed, ok := facts.ChildFinalOutcome(ctx, req.ChildSessionID, batchdispatch.AsyncCompletion{
		SessionID: req.ChildSessionID, ToolCallID: req.ToolCallID, ToolName: req.Tool,
		Content: response.Content, IsError: response.IsError,
	})
	if !ok {
		return response, false
	}
	// A refreshed outcome replaces the captured response entirely; the
	// initial turn's task_outcome declaration is not carried over — the
	// child's final turn supersedes it.
	return fantasy.ToolResponse{Content: refreshed.Content, IsError: refreshed.IsError}, true
}
