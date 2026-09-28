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

import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/permission"
)

// subAgentDriver is a frozen snapshot of the SessionAgentCall shape
// runSubAgent used to start a delegated child's turn, plus the SessionAgent
// it ran on. callFor reuses both for a later wake: the SAME agent (so the
// wake queues into, or restarts, the correct mailbox) and the SAME
// model/provider/sampling shape the delegation was launched with -- only the
// prompt changes.
type subAgentDriver struct {
	agent SessionAgent
	call  SessionAgentCall
	// parentSessionID is the delegating session that originally armed this
	// child's restricted-run allowlist baseline (runSubAgent's
	// InheritSessionRunAllowlistForGeneration). wakeSession re-inherits from
	// it on EVERY wake (§6.2), not just the first turn: the child's own turn
	// no longer clears its allowlist entry on return (coordinator_subagents.go/
	// async_tool.go dropped their `defer Clear`), so re-arming here keeps a
	// woken turn judged by the delegation's policy instead of falling back
	// to the process-wide gate. Empty for a driver registered without a
	// known parent (should not happen in production; a nil-safe no-op).
	parentSessionID string
	// generation is THIS record's own monotonic registration id, assigned by
	// register -- not the mailbox ownership epoch, not a turn's
	// LogicalCallID. Phase 3 (§6.2) uses it to compare-and-delete this
	// record (releaseIfCurrent) and to bind the allowlist entry inherited
	// under it (InheritSessionRunAllowlistForGeneration/
	// ClearSessionRunAllowlistForGeneration, permission.go), the same idiom
	// SetSessionRunAllowlistForEpoch/ClearSessionRunAllowlistForEpoch
	// already use for the mailbox epoch: a stale release from an OLD
	// generation can never delete a NEWER resume_session_id's registration
	// or its allowlist entry.
	generation uint64
}

// callFor returns a copy of the driver's call template with prompt as its
// new Prompt; call kind and notice flags are reset (callFromActive).
func (d subAgentDriver) callFor(prompt string) SessionAgentCall {
	call := callFromActive(d.call)
	call.Prompt = prompt
	return call
}

// subAgentDriverRegistry maps a delegated child session id to the driver
// that owns its turns. Entries are written by runSubAgent right before it
// starts (or resumes) the child's turn.
//
// Phase 1/2 kept every entry for the coordinator's whole lifetime, on the
// theory that the `agent` tool's resume_session_id path can re-drive the
// SAME child session arbitrarily far in the future and dropping the entry
// early would silently fall back to c.currentAgent for that later resume --
// reproducing task #1049 for old sessions. Phase 3 (§6.2) removes an entry
// once its child's scope is CONFIRMED closed (releaseDriverIfScopeClosed,
// called from recheckChild), which does not reopen that risk: a later
// resume_session_id always goes through runSubAgent's own register() call
// again BEFORE the child's next turn runs (proven by
// TestRunSubAgent_ResumeAfterScopeClosedReArmsAllowlist), so a resumed
// session is never left driverless -- it just no longer holds the entry
// during the (possibly very long) idle window in between.
type subAgentDriverRegistry struct {
	mu      sync.Mutex
	byChild map[string]subAgentDriver
	// nextGen is the monotonic source for subAgentDriver.generation, mirroring
	// mailbox.epoch's own "never issue 0" convention (beginCompact bumps an
	// idle mailbox to 1) so a zero-value subAgentDriver (never registered)
	// can never be mistaken for a real generation.
	nextGen uint64
}

func newSubAgentDriverRegistry() *subAgentDriverRegistry {
	return &subAgentDriverRegistry{byChild: make(map[string]subAgentDriver)}
}

// register records (or, on a resume_session_id re-delegation, replaces) the
// driver for childSessionID, and returns the generation this record was just
// assigned. Callers that need to bind a LATER release/clear to exactly THIS
// registration (§6.2) capture the return value here rather than re-reading
// get() a moment later, which could already observe a concurrent
// re-registration. Nil-receiver safe so bare test fixtures that never build
// one keep working (returns 0, a generation releaseIfCurrent can never match
// since register always assigns >= 1).
func (r *subAgentDriverRegistry) register(childSessionID string, driver subAgentDriver) uint64 {
	if r == nil || childSessionID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextGen++
	driver.generation = r.nextGen
	r.byChild[childSessionID] = driver
	return driver.generation
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

// releaseIfCurrent removes childSessionID's driver record ONLY if it is
// still on generation g -- the same compare-and-delete idiom
// permission.go's ClearSessionRunAllowlistForEpoch already applies to a
// different map. A concurrent resume_session_id that re-registered
// childSessionID (bumping its generation) always wins: this returns false
// and touches nothing, leaving the newer record exactly as it was.
func (r *subAgentDriverRegistry) releaseIfCurrent(childSessionID string, g uint64) bool {
	if r == nil || childSessionID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.byChild[childSessionID]; ok && current.generation == g {
		delete(r.byChild, childSessionID)
		return true
	}
	return false
}

// agentFor resolves the SessionAgent that actually owns sessionID's mailbox:
// the registered delegation driver, if any (a delegated child session runs
// on its own task SessionAgent, never on c.currentAgent -- task #1049), else
// c.currentAgent for every non-delegated session. Single choke point for
// every coordinator method that dispatches a call/cancel/inject onto
// "whichever SessionAgent owns this session id" -- wakeSession, Cancel,
// InjectMessage (task #1054) all call this instead of each re-implementing
// the same fallback.
func (c *coordinator) agentFor(sessionID string) SessionAgent {
	if driver, ok := c.subAgentDrivers.get(sessionID); ok {
		return driver.agent
	}
	return c.currentAgent
}

// releaseDriverIfScopeClosed removes childSessionID's driver record and its
// restricted-run allowlist entry once the child's scope is confirmed closed
// (§6.1's diagnosis: neither was ever cleared before phase 3, growing
// without bound for the coordinator's whole lifetime -- phase 2 explicitly
// deferred this cleanup here). Called from recheckChild's tail (work_ledger_
// delegation.go, §5) at the exact moment it confirms nothing more is armed
// for childSessionID -- including every session that was NEVER a delegated
// child at all (trigger (iv)/the by-construction onSessionIdle hook call
// this for every session's run-end, not just delegated ones): get() returns
// ok=false for those and this returns immediately, no side effects.
//
// Safe against the resume_session_id race (§6.3): releaseIfCurrent compares
// against the CURRENT map entry under its own lock, not a snapshot taken
// earlier in this call -- a concurrent re-registration (a resume landing in
// the same window) always wins and this becomes a no-op. Clearing the
// allowlist entry under the SAME generation closes the matching window
// there too (orchestrator decision 2026-09-28 item 2: closed by
// construction, not accepted as residual risk).
func (c *coordinator) releaseDriverIfScopeClosed(childSessionID string) {
	driver, ok := c.subAgentDrivers.get(childSessionID)
	if !ok {
		return
	}
	if !c.subAgentDrivers.releaseIfCurrent(childSessionID, driver.generation) {
		return // a concurrent resume_session_id already re-registered -- not ours to release
	}
	if mgr, ok := c.permissions.(permission.SessionRunAllowlistManager); ok {
		mgr.ClearSessionRunAllowlistForGeneration(childSessionID, driver.generation)
	}
}
