// Coordinator-side helpers for the work ledger: transitive "is this session
// (or anything below it) still owning work" queries, the delegation
// completion refresh, and the parked-delegation status reporter. Moved out of
// subagent_outcome.go (deleted by phase 1, docs/plans/2026-09-27-async-
// phase1-spec.md) unchanged in behavior -- these stay coordinator methods
// (refreshSubAgentCompletion needs c.messages) rather than moving onto
// workLedger itself.
package agent

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
)

// refreshSubAgentCompletion re-reads childSessionID's newest finished
// assistant message and folds its state into the completion about to be
// handed to the parent. Content and IsError are both taken from that message
// when one exists, so:
//
//   - a child whose last turn ended cleanly delivers "finished";
//   - a child whose last turn errored (FinishReasonError -- e.g. its own
//     background job's failure surfacing through the auto-resume turn)
//     delivers "failed";
//   - a child that never produced a finished assistant message keeps the
//     completion captured at arm time (an error return from the child's run,
//     or a panic in the tool).
//
// A DB failure keeps the captured completion rather than dropping the
// notice: losing the notice is the worse outcome, and the captured value is
// still the child's own last turn.
func (c *coordinator) refreshSubAgentCompletion(childSessionID string, completion AsyncCompletion) AsyncCompletion {
	if c.messages == nil || childSessionID == "" {
		return completion
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := c.messages.List(ctx, childSessionID)
	if err != nil {
		slog.Debug("sub-agent outcome refresh failed, using captured completion",
			"child_session", childSessionID, "err", err)
		return completion
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if msg.Role != message.Assistant || !msg.IsFinished() {
			continue
		}
		if text := strings.TrimSpace(msg.FullText()); text != "" {
			completion.Content = tools.TruncateOutput(text)
		}
		completion.IsError = msg.FinishReason() == message.FinishReasonError
		return completion
	}
	return completion
}

// DescendantWorkPending reports whether sessionID, or ANY session below it in
// the parent->child session tree, still owns work that has not reached a
// terminal state. This is the transitive form root-terminality needs: an
// intermediate end_turn/yield after delegation is NOT completion, so the
// root session must be held running/waiting until every depth of its
// delegation tree is terminal.
//
// "Still owns work" means any of:
//
//   - an async job registered to that session that has not been delivered
//     (workLedger.running), or
//   - a background shell owned by that session that is still running
//     (BackgroundShellManager.ActiveOwned), or
//   - a delegation armed for that session that has not been released yet
//     (workLedger's byChild index), either as the parent that has not been
//     told, or as the child whose run has not come back.
//
// The walk is deliberately guarded by a fast in-memory pre-check: when
// nothing is pending anywhere, there is no DB access at all, so the common
// "everything finished" case stays free. Only when the pre-check says
// something IS pending does this touch the sessions table, and then it
// walks breadth-first from the root with a visited set so a corrupt or
// cyclic linkage cannot loop.
//
// Deliberately NOT consulted: the child's mailbox ownership (IsSessionBusy).
// That is the resume path's state, not work; gating on it would wedge
// `rush run --session <id>`.
func (c *coordinator) DescendantWorkPending(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	// Fast path: nothing pending process-wide, so nothing can be pending
	// below this session either. No DB read.
	if !c.anyPendingWorkInMemory() {
		return false
	}
	if c.sessions == nil {
		// Without the session table we cannot walk the tree, so the safest
		// answer is the one that does not strand a running workflow: report
		// pending only for the session we were asked about.
		return c.sessionOwnsPendingWork(sessionID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	visited := make(map[string]struct{})
	queue := []string{sessionID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, seen := visited[current]; seen || current == "" {
			continue
		}
		visited[current] = struct{}{}

		if c.sessionOwnsPendingWork(current) {
			return true
		}
		children, err := c.sessions.ListSubSessions(ctx, current)
		if err != nil {
			// A failed read must not silently clear the gate. Fall back to
			// the conservative answer for the sessions already visited.
			slog.Debug("DescendantWorkPending: child listing failed", "session", current, "err", err)
			return true
		}
		for _, child := range children {
			if _, seen := visited[child.ID]; !seen {
				queue = append(queue, child.ID)
			}
		}
	}
	return false
}

// anyPendingWorkInMemory is the cheap pre-check: is there ANY pending async
// job, live background shell, or unreleased armed delegation anywhere in
// this process? Touches only in-memory registries.
func (c *coordinator) anyPendingWorkInMemory() bool {
	if c.asyncJobs != nil && c.asyncJobs.anyRunning() {
		return true
	}
	if c.background != nil && c.background.ActiveJobs() > 0 {
		return true
	}
	if c.asyncJobs != nil && c.asyncJobs.hasParked() {
		return true
	}
	return false
}

// sessionOwnsPendingWork is the single-session form of
// DescendantWorkPending: does THIS session own pending work?
func (c *coordinator) sessionOwnsPendingWork(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	if c.asyncJobs != nil && c.asyncJobs.running(sessionID) {
		return true
	}
	if c.background != nil && c.background.ActiveOwned(sessionID) > 0 {
		return true
	}
	if c.asyncJobs != nil && c.asyncJobs.hasParkedFor(sessionID) {
		return true
	}
	return false
}

// noteSubAgentChildRunEnded is re-check trigger (iv): a run on sessionID has
// just returned, so a release that was deferred by childScopeDrained's busy
// check can now be re-evaluated. Thin wrapper kept under its existing name
// for a minimal diff at its three call sites (coordinator_run.go,
// coordinator_background.go).
func (c *coordinator) noteSubAgentChildRunEnded(sessionID string) {
	if c.asyncJobs == nil {
		return
	}
	c.asyncJobs.recheckChild(sessionID)
}

// parkedParentSessions returns the distinct parent session ids that still
// have at least one delegation armed. See ParkedSubAgentWorkReporter.
func (c *coordinator) parkedParentSessions() []string {
	if c.asyncJobs == nil {
		return nil
	}
	return c.asyncJobs.parkedParentSessions()
}

// ParkedSubAgentWorkReporter is the OPTIONAL interface *coordinator exposes
// so a status classifier can tell "session at rest, nothing outstanding"
// apart from "session at rest, but a delegated sub-agent outcome is still
// parked".
//
// It is deliberately NOT on the Coordinator interface: every mock
// implementing that interface in internal/server and elsewhere would have to
// grow a method. Callers type-assert instead, and a value that does not
// implement it simply means "no parked-delegation signal available".
type ParkedSubAgentWorkReporter interface {
	// ParkedSubAgentParents returns the parent session ids that currently
	// have at least one delegated sub-agent outcome parked. Empty (or a
	// value not implementing this interface) means nothing is parked.
	ParkedSubAgentParents() []string
}

// ParkedSubAgentParents implements ParkedSubAgentWorkReporter.
func (c *coordinator) ParkedSubAgentParents() []string {
	return c.parkedParentSessions()
}
