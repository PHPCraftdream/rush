// Sub-agent delegation and skill discovery: bodies moved to
// subagents_component.go (the subagentsComponent host pattern); this file
// keeps the thin coordinator delegates.
package agent

import (
	"context"

	"charm.land/fantasy"
)

func (c *coordinator) runSubAgent(ctx context.Context, params subAgentParams) (fantasy.ToolResponse, error) {
	return newSubagentsComponent(c).runSubAgent(ctx, params)
}

func (c *coordinator) inheritBatchQuestionState(ctx context.Context, childSessionID string, call *SessionAgentCall) error {
	return newSubagentsComponent(c).inheritBatchQuestionState(ctx, childSessionID, call)
}

func (c *coordinator) agentFor(sessionID string) SessionAgent {
	return newSubagentsComponent(c).agentFor(sessionID)
}

func (c *coordinator) releaseDriverIfScopeClosed(childSessionID string) {
	newSubagentsComponent(c).releaseDriverIfScopeClosed(childSessionID)
}

func (c *coordinator) releaseDriverGeneration(childSessionID string, generation uint64) {
	newSubagentsComponent(c).releaseDriverGeneration(childSessionID, generation)
}
