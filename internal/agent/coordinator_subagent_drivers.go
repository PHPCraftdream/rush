// Per-delegated-child-session driver registry.
//
// A delegated sub-agent's turn runs on its own *sessionAgent* -- the task
// agent runSubAgent's caller builds via coordinator.agentTool/buildAgent --
// with its own mailbox and in-process session reservation, completely
// separate from c.currentAgent (the root/interactive coder agent). Two
// SessionAgent instances trying to acquire the SAME child session's OS-level
// lock (internal/session.SessionLock) in the same process race each other:
// the lock only tracks "does THIS process already hold it", not which
// SessionAgent asked for it. If a wake for the child's own async-job or
// background-job completion (notifyAsyncCompletion, coordinator_background.go)
// is dispatched through c.Run -> c.currentAgent.Run while the task agent's
// own Run for that exact child session is still in flight (mid-stream, still
// holding the lock), the coder agent's mailbox has no record of the child
// session at all, treats it as idle, and loses that race outright: "session
// ... is already in use", holder_pid = this same process. See task #1049.
//
// The fix ("one driver per session", design doc
// docs/plans/2026-09-27-async-structured-concurrency.md §3): every wake for
// a delegated child session must be dispatched through the SAME SessionAgent
// that is driving it. That agent's own mailbox either queues the wake behind
// its live turn (drained by that turn's own end-of-turn loop, agent_run.go's
// runOwned) or, once idle, starts it directly -- either way there is only
// ever one claimant for the child's OS lock.
package agent

import "sync"

// subAgentDriver is a frozen snapshot of the SessionAgentCall shape
// runSubAgent used to start a delegated child's turn, plus the SessionAgent
// it ran on. callFor reuses both for a later wake: the SAME agent (so the
// wake queues into, or restarts, the correct mailbox) and the SAME
// model/provider/sampling shape the delegation was launched with -- only the
// prompt changes.
type subAgentDriver struct {
	agent SessionAgent
	call  SessionAgentCall
}

// callFor returns a copy of the driver's call template with prompt as its
// new Prompt.
func (d subAgentDriver) callFor(prompt string) SessionAgentCall {
	call := d.call
	call.Prompt = prompt
	return call
}

// subAgentDriverRegistry maps a delegated child session id to the driver
// that owns its turns. Entries are written by runSubAgent right before it
// starts (or resumes) the child's turn, and are kept for the coordinator's
// lifetime rather than removed once the child goes idle: the `agent` tool's
// resume_session_id path can re-drive the SAME child session arbitrarily far
// in the future, long after any parked outcome for it was released, and
// dropping the entry early would silently fall back to c.currentAgent for
// that later resume -- reproducing this exact bug for old sessions. Each
// entry is one interface value plus one SessionAgentCall snapshot -- the
// same order of growth as coordinator.agents and the session/message tables
// themselves, neither of which is pruned in-memory either -- and the
// SessionAgent value it points to is itself one of a small number of
// long-lived, process-wide objects (buildAgent's task-agent build), not one
// object per delegation.
type subAgentDriverRegistry struct {
	mu      sync.Mutex
	byChild map[string]subAgentDriver
}

func newSubAgentDriverRegistry() *subAgentDriverRegistry {
	return &subAgentDriverRegistry{byChild: make(map[string]subAgentDriver)}
}

// register records (or, on a resume_session_id re-delegation, replaces) the
// driver for childSessionID. Nil-receiver safe so bare test fixtures that
// never build one keep working.
func (r *subAgentDriverRegistry) register(childSessionID string, driver subAgentDriver) {
	if r == nil || childSessionID == "" {
		return
	}
	r.mu.Lock()
	r.byChild[childSessionID] = driver
	r.mu.Unlock()
}

// get returns the registered driver for childSessionID, if any.
func (r *subAgentDriverRegistry) get(childSessionID string) (subAgentDriver, bool) {
	if r == nil || childSessionID == "" {
		return subAgentDriver{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.byChild[childSessionID]
	return d, ok
}
