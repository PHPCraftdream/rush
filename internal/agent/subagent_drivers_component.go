// Sub-agent driver lookup and release methods for the subagents component.
package agent

import (
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
)

// agentFor resolves the SessionAgent that actually owns sessionID's mailbox:
// the registered delegation driver, if any (a delegated child session runs
// on its own task SessionAgent, never on the coordinator's current agent), else
// the current agent for every non-delegated session.
func (c *subagentsComponent) agentFor(sessionID string) SessionAgent {
	if driver, ok := c.host.subAgentDrivers().Get(sessionID); ok {
		return driver.Agent
	}
	return c.host.currentAgent()
}

// releaseDriverIfScopeClosed removes a child's driver and generation-bound
// allowlist entry once the child's scope is confirmed closed.
func (c *subagentsComponent) releaseDriverIfScopeClosed(childSessionID string) {
	driver, ok := c.host.subAgentDrivers().Get(childSessionID)
	if !ok {
		return
	}
	c.releaseDriverGeneration(childSessionID, driver.Generation)
}

// releaseDriverGeneration removes the record only while its registration
// generation remains current; stale closure observations cannot remove a
// concurrent resume's replacement driver.
func (c *subagentsComponent) releaseDriverGeneration(childSessionID string, generation uint64) {
	if !c.host.subAgentDrivers().ReleaseIfCurrent(childSessionID, generation) {
		return // a concurrent resume_session_id already re-registered -- not ours to release
	}
	if mgr, ok := c.host.permissions().(permission.SessionRunAllowlistManager); ok {
		mgr.ClearSessionRunAllowlistForGeneration(childSessionID, generation)
	}
}
