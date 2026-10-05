// Sub-agent control tools (inspect_agent/inject_agent/stop_agent,
// docs/plans/2026-09-27-wake-tools-contract.md §4). The tool layer is thin:
// it reads the caller session from context and delegates to AgentControl,
// implemented by the agent package's coordinator (see
// coordinator_agent_control.go) so every ownership check, status signal and
// cancel/inject route through the same coordinator paths the rest of the
// async machinery uses.
package tools

import (
	"context"
	"fmt"
)

// AgentControl is the coordinator-side half of the sub-agent control tools.
// Every returned error's message is model-safe (returned via
// NewTextErrorResponse, not a fatal Go error).
type AgentControl interface {
	// InspectAgent reports a delegated child's status (§4.1).
	InspectAgent(ctx context.Context, callerSessionID, childSessionID string) (AgentInspection, error)
	// ListDelegations lists the caller session's live delegations (§4.1's
	// no-argument inspect_agent listing). A delivered delegation is gone
	// from the ledger by design and is not listed.
	ListDelegations(ctx context.Context, callerSessionID string) ([]AgentDelegationSummary, error)
	// InjectAgent delivers msg into the child's session (§4.2) and returns
	// the tool's own answer text.
	InjectAgent(ctx context.Context, callerSessionID, childSessionID, msg string, interrupt bool) (string, error)
	// StopAgent stops the child's current turn and armed delegation (§4.3)
	// and returns the tool's own answer text.
	StopAgent(ctx context.Context, callerSessionID, childSessionID string) (string, error)
}

// AgentInspection is inspect_agent's JSON output (§4.1).
type AgentInspection struct {
	ChildSessionID      string `json:"child_session_id"`
	Status              string `json:"status"`
	QueuedMessages      int    `json:"queued_messages"`
	LastActivityAt      string `json:"last_activity_at,omitempty"`
	LastActivitySummary string `json:"last_activity_summary,omitempty"`
}

// AgentDelegationSummary is one live delegation in ListDelegations' output.
type AgentDelegationSummary struct {
	ChildSessionID string `json:"child_session_id"`
	ToolName       string `json:"tool_name"`
	Status         string `json:"status"`
	StartedAt      string `json:"started_at,omitempty"`
	AgeSeconds     int    `json:"age_seconds,omitempty"`
	LastActivityAt string `json:"last_activity_at,omitempty"`
}

// childSessionIDParam is the shared ownership-checked parameter.
type childSessionIDParam struct {
	ChildSessionID string `json:"child_session_id" description:"The delegated sub-agent's session id (the id the agent tool reported when the delegation started)."`
}

func childSessionFromParams(params childSessionIDParam) (string, error) {
	if params.ChildSessionID == "" {
		return "", fmt.Errorf("child_session_id is required: pass the id the agent tool reported for the delegation, or call inspect_agent with no arguments to list your live delegations")
	}
	return params.ChildSessionID, nil
}
