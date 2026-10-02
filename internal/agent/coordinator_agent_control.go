// Coordinator-side implementation of tools.AgentControl — the
// inspect_agent/inject_agent/stop_agent tool family (task #1024, stage 3 of
// docs/plans/2026-09-24-agent-wakes-and-async-job-control.md; contract:
// docs/plans/2026-09-27-wake-tools-contract.md §4). Every method routes
// through agentFor (task #1054), so a delegated child's driver SessionAgent —
// not c.currentAgent — answers busy checks, injects and cancels.
package agent

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
)

var _ tools.AgentControl = (*coordinator)(nil)

// controlChild is the shared ownership check (§4): the child must exist and
// be a direct child of the caller's session. Same wording as runSubAgent's
// resume_session_id check, with the action verb swapped in.
func (c *coordinator) controlChild(ctx context.Context, callerSessionID, childSessionID, verb string) error {
	child, err := c.sessions.Get(ctx, childSessionID)
	if err != nil {
		return fmt.Errorf("child_session_id %q not found: %s", childSessionID, err)
	}
	if child.ParentSessionID != callerSessionID {
		return fmt.Errorf(
			"child_session_id %q is not a child of this session; refusing to %s a session that does not belong to this agent call",
			childSessionID, verb,
		)
	}
	return nil
}

// childDelegationState snapshots the armed delegation ledger entry for
// childID (the `agent`/`agentic_fetch` call parked in byChild[childID]).
// ok is false when no entry exists — a child that was never delegated, or
// one whose entry was already delivered and gc'd (gcChildLocked drops
// terminal entries, so a delivered delegation is invisible here by design;
// inspect_agent then falls back to the child's message history).
func (c *coordinator) childDelegationState(childID string) (state jobPhase, ok bool) {
	if c.asyncJobs == nil {
		return 0, false
	}
	c.asyncJobs.mu.Lock()
	defer c.asyncJobs.mu.Unlock()
	entries := c.asyncJobs.byChild[childID]
	if len(entries) == 0 {
		return 0, false
	}
	job := entries[len(entries)-1]
	return job.state, true
}

// childHasActiveWork reports whether childID has anything stop_agent could
// stop: a live turn (via its driver-aware busy check) or a running ledger
// entry — either the delegation parked on it as a child, or work it owns
// itself (its own bash/run_command jobs).
func (c *coordinator) childHasActiveWork(childID string) bool {
	if c.agentFor(childID).IsSessionBusy(childID) {
		return true
	}
	if c.asyncJobs == nil {
		return false
	}
	c.asyncJobs.mu.Lock()
	defer c.asyncJobs.mu.Unlock()
	if s := c.asyncJobs.bySession[childID]; s != nil {
		for _, job := range s.jobs {
			if job.state == phaseRunning {
				return true
			}
		}
	}
	for _, job := range c.asyncJobs.byChild[childID] {
		if job.state == phaseRunning {
			return true
		}
	}
	return false
}

// childLastActivity reads the child's last message for inspect_agent's
// last_activity_* fields (the same List refreshSubAgentCompletion uses) and
// reports whether that message carries the exact question-stop finish
// literal (§4.1's awaiting_answer signal).
func (c *coordinator) childLastActivity(ctx context.Context, childID string) (msg *message.Message, awaiting bool) {
	msgs, err := c.messages.List(ctx, childID)
	if err != nil || len(msgs) == 0 {
		return nil, false
	}
	last := msgs[len(msgs)-1]
	fp := last.FinishPart()
	awaiting = fp != nil && fp.Reason == message.FinishReasonError &&
		fp.Message == awaitingAnswerStoppedTitle
	return &last, awaiting
}

// InspectAgent implements tools.AgentControl (§4.1). Priority: running
// (driver-aware busy) beats everything; then awaiting_answer (the exact
// question-stop finish literal); then a terminal registry state; then
// armed-but-not-running (idle); then a history-derived terminal for an
// already-delivered (gc'd) delegation; then idle for a never-run child.
func (c *coordinator) InspectAgent(ctx context.Context, callerSessionID, childSessionID string) (tools.AgentInspection, error) {
	if err := c.controlChild(ctx, callerSessionID, childSessionID, "inspect"); err != nil {
		return tools.AgentInspection{}, err
	}
	insp := tools.AgentInspection{ChildSessionID: childSessionID}
	last, awaiting := c.childLastActivity(ctx, childSessionID)
	switch {
	case c.agentFor(childSessionID).IsSessionBusy(childSessionID):
		insp.Status = "running"
	case awaiting:
		insp.Status = "awaiting_answer"
	default:
		state, ok := c.childDelegationState(childSessionID)
		if ok && state.terminal() {
			insp.Status = terminalStatusFor(state)
		} else if ok {
			insp.Status = "idle"
		} else if last != nil {
			// Delivered (gc'd) delegation: derive the terminal state from
			// the last message's finish reason.
			insp.Status = finishedStatusFor(last)
		} else {
			insp.Status = "idle"
		}
	}
	if ag := c.agentFor(childSessionID); ag != nil {
		insp.QueuedMessages = len(ag.QueuedPromptsList(childSessionID))
	}
	if last != nil {
		insp.LastActivityAt = time.Unix(last.CreatedAt, 0).UTC().Format(time.RFC3339)
		insp.LastActivitySummary = tools.TruncateOutput(last.FullText())
	}
	return insp, nil
}

// ListDelegations implements tools.AgentControl: the no-argument
// inspect_agent listing. Live means the caller still owns the delegation in
// the work ledger (bySession[caller].jobs with a non-empty childSession);
// a delivered delegation is gone from the ledger by design and is not
// listed. Ordered oldest-first by start time.
func (c *coordinator) ListDelegations(ctx context.Context, callerSessionID string) ([]tools.AgentDelegationSummary, error) {
	if c.asyncJobs == nil {
		return nil, nil
	}
	type liveEntry struct {
		childID   string
		toolName  string
		startedAt time.Time
	}
	c.asyncJobs.mu.Lock()
	var entries []liveEntry
	if s := c.asyncJobs.bySession[callerSessionID]; s != nil {
		for _, job := range s.jobs {
			if job.childSession != "" {
				entries = append(entries, liveEntry{childID: job.childSession, toolName: job.toolName, startedAt: job.startedAt})
			}
		}
	}
	c.asyncJobs.mu.Unlock()
	// Oldest first, with the child id breaking ties: two delegations armed
	// inside the same clock tick read an equal startedAt (the process clock
	// is far coarser than the gap between two Start calls), and an unstable
	// sort would then list them in random map order.
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].startedAt.Equal(entries[j].startedAt) {
			return entries[i].startedAt.Before(entries[j].startedAt)
		}
		return entries[i].childID < entries[j].childID
	})
	summaries := make([]tools.AgentDelegationSummary, 0, len(entries))
	for _, e := range entries {
		insp, err := c.InspectAgent(ctx, callerSessionID, e.childID)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, tools.AgentDelegationSummary{
			ChildSessionID: e.childID,
			ToolName:       e.toolName,
			Status:         insp.Status,
			StartedAt:      e.startedAt.UTC().Format(time.RFC3339),
			AgeSeconds:     int(time.Since(e.startedAt).Seconds()),
			LastActivityAt: insp.LastActivityAt,
		})
	}
	return summaries, nil
}

func terminalStatusFor(state jobPhase) string {
	switch state {
	case phaseCompleted:
		return "finished"
	case phaseFailed:
		return "failed"
	case phaseCancelled:
		return "cancelled"
	case phaseTimedOut:
		return "timed_out"
	default:
		return "running"
	}
}

// finishedStatusFor derives a terminal inspect status from a delivered
// delegation's last child message.
func finishedStatusFor(msg *message.Message) string {
	if fp := msg.FinishPart(); fp != nil {
		switch fp.Reason {
		case message.FinishReasonError:
			return "failed"
		case message.FinishReasonCanceled:
			return "cancelled"
		}
	}
	return "finished"
}

// InjectAgent implements tools.AgentControl (§4.2).
func (c *coordinator) InjectAgent(ctx context.Context, callerSessionID, childSessionID, msg string, interrupt bool) (string, error) {
	if err := c.controlChild(ctx, callerSessionID, childSessionID, "inject"); err != nil {
		return "", err
	}
	busy := c.agentFor(childSessionID).IsSessionBusy(childSessionID)
	if !interrupt {
		if _, err := c.InjectMessage(ctx, childSessionID, msg); err != nil {
			return "", err
		}
		if busy {
			return "Message delivered into the sub-agent's running turn; it merges at the next step boundary.", nil
		}
		return fmt.Sprintf(
			"Message saved to the sub-agent's session; it is idle, so nothing runs until you call agent with resume_session_id=%q.",
			childSessionID,
		), nil
	}
	pinned, err := c.resolveSessionModels(ctx, childSessionID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve session models for inject: %w", err)
	}
	call, err := c.buildCall(ctx, childSessionID, msg, pinned, nil)
	if err != nil {
		return "", err
	}
	if c.agentFor(childSessionID).InterruptAndReplace(childSessionID, call) {
		return "Interrupted the sub-agent's current turn; it will process this message next.", nil
	}
	// Idle: nothing to interrupt — fall back to the queue behavior and say so.
	if _, err := c.InjectMessage(ctx, childSessionID, msg); err != nil {
		return "", err
	}
	return "child session was idle, nothing to interrupt — message queued instead; call agent with resume_session_id to resume it", nil
}

// StopAgent implements tools.AgentControl (§4.3): ledger cancellation via
// the existing cancelSession(childID) (captures both the delegation parked on
// the child and any work the child owns, and delivers the parent exactly one
// cancelled notice) plus the driver-aware Cancel for the child's live
// generation. Idempotent: a second call finds nothing left to stop.
func (c *coordinator) StopAgent(ctx context.Context, callerSessionID, childSessionID string) (string, error) {
	if err := c.controlChild(ctx, callerSessionID, childSessionID, "stop"); err != nil {
		return "", err
	}
	if !c.childHasActiveWork(childSessionID) {
		return "", fmt.Errorf("child_session_id %q has no active turn or pending delegation to stop", childSessionID)
	}
	c.Cancel(childSessionID)
	return fmt.Sprintf(
		"Sub-agent session %s stopped. Its history is preserved; resume with agent(resume_session_id=%q, prompt=\"...\") if needed.",
		childSessionID, childSessionID,
	), nil
}
