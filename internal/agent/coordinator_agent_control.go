// Coordinator-side implementation of tools.AgentControl — the
// inspect_agent/inject_agent/stop_agent tool family (task #1024, stage 3 of
// docs/plans/2026-09-24-agent-wakes-and-async-job-control.md; contract:
// docs/plans/2026-09-27-wake-tools-contract.md §4). Every method routes
// through agentFor (task #1054), so a delegated child's driver SessionAgent —
// not c.currentAgent — answers busy checks, injects and cancels.
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"charm.land/fantasy"
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

// childDelegationState reports childID's LIVE delegation state from the
// batch facts: "running" when the parent's live tree still names childID as
// its delegation child. ok is false when no live delegation exists — a
// child that was never delegated, or one whose delegation already delivered
// (invisible here by design; inspect_agent then falls back to the child's
// message history).
func (c *coordinator) childDelegationState(ctx context.Context, childID string) (state string, ok bool) {
	parent := c.childParentSession(childID)
	if parent == "" {
		return "", false
	}
	live, err := coordinatorBatchCutoverFacts(c).LiveDelegationChildOf(ctx, parent, childID)
	if err != nil || !live {
		return "", false
	}
	return "running", true
}

// childHasActiveWork reports whether childID has anything stop_agent could
// stop: a live turn (via its driver-aware busy check), live batch work it
// owns (its own task roots), or owned background shells. A failed live read
// is conservative (active).
func (c *coordinator) childHasActiveWork(childID string) bool {
	if c.agentFor(childID).IsSessionBusy(childID) {
		return true
	}
	own, desc, err := coordinatorBatchCutoverFacts(c).LiveWorkSummary(context.Background(), childID)
	if err != nil || own+desc > 0 {
		return true
	}
	return c.background != nil && (c.background.ActiveOwned(childID) > 0 || c.background.PendingCompletionsOwned(childID) > 0)
}

// childLastActivity reads the child's last FINISHED assistant message for
// inspect_agent's last_activity_* fields (the same List and the same
// finished-assistant scan refreshSubAgentCompletion uses) and reports
// whether it ends on a question-tool stop (§4.1's awaiting_answer signal).
// #1157: the literal "last message" of a paused child is its ask_question
// tool RESULT -- a tool message with no finish part -- which made a
// question-paused child read as idle; the question lives on the assistant
// message before it.
func (c *coordinator) childLastActivity(ctx context.Context, childID string) (msg *message.Message, awaiting bool) {
	msgs, err := c.messages.List(ctx, childID)
	if err != nil {
		return nil, false
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role != message.Assistant || !m.IsFinished() {
			continue
		}
		fp := m.FinishPart()
		awaiting = fp != nil && fp.Reason == message.FinishReasonError &&
			fp.Message == awaitingAnswerStoppedTitle
		return &m, awaiting
	}
	return nil, false
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
		state, ok := c.childDelegationState(ctx, childSessionID)
		if ok {
			// §4.1: "running" means the child executes a turn (the busy case
			// above). A live delegation whose child is between turns is idle.
			if state == "running" {
				state = "idle"
			}
			insp.Status = state
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
// the batch facts (a live delegation node naming a child session); a
// delivered delegation is terminal in the facts by design and is not
// listed. Ordered oldest-first by start time.
func (c *coordinator) ListDelegations(ctx context.Context, callerSessionID string) ([]tools.AgentDelegationSummary, error) {
	nodes, err := coordinatorBatchCutoverFacts(c).LiveDelegationNodes(ctx, callerSessionID)
	if err != nil {
		return nil, err
	}
	type liveEntry struct {
		childID   string
		toolName  string
		startedAt time.Time
	}
	entries := make([]liveEntry, 0, len(nodes))
	for _, node := range nodes {
		if node.ChildSessionID == "" {
			continue
		}
		toolName := node.TaskKind
		if toolName == "" {
			toolName = "agent"
		}
		entries = append(entries, liveEntry{childID: node.ChildSessionID, toolName: toolName, startedAt: node.StartedAt})
	}
	// Oldest first, with the child id breaking ties: two delegations armed
	// inside the same clock tick read an equal startedAt (the process clock
	// is far coarser than the gap between two launches), and an unstable
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
		// Batch held-question bridge (D1): a committed, CURRENT-incarnation
		// held question of this child releases through the node's execution
		// (Runner.Inject) instead of the mailbox. A sink-less coordinator or
		// a "no held question" answer keeps the legacy path byte-identical.
		fact, batchErr := c.deliverBatchHeldAnswer(ctx, callerSessionID, childSessionID, msg)
		if batchErr == nil {
			return fmt.Sprintf(
				"Answer delivered to the held question of the existing batch node %s (token %d, depth %d); the sub-agent continues in the SAME batch execution - no new generation.",
				fact.NodeID, fact.Token, fact.Depth,
			), nil
		}
		if !errors.Is(batchErr, errBatchInjectBridgeMiss) {
			return "", batchErr
		}
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
	ctx, ownedScope := ownDeferredCallScope(ctx)
	scopeTransferred := false
	if ownedScope != nil {
		defer func() {
			if !scopeTransferred {
				ownedScope.close()
			}
		}()
	}
	pinned, err := c.resolveSessionModels(ctx, childSessionID)
	if err != nil {
		return "", fmt.Errorf("failed to resolve session models for inject: %w", err)
	}
	call, err := c.buildCall(ctx, childSessionID, msg, pinned, nil)
	if err != nil {
		return "", err
	}
	if ownedScope != nil {
		call.onQueueResolved = func(_ *fantasy.AgentResult, _ error) { ownedScope.close() }
	}
	if c.agentFor(childSessionID).InterruptAndReplace(childSessionID, call) {
		scopeTransferred = true
		return "Interrupted the sub-agent's current turn; it will process this message next.", nil
	}
	// Idle fallback does not accept this built call; release its privately
	// resolved scope before preserving the historical InjectMessage behavior.
	if ownedScope != nil {
		ownedScope.close()
		scopeTransferred = true
	}
	// Idle: nothing to interrupt — fall back to the queue behavior and say so.
	if _, err := c.InjectMessage(ctx, childSessionID, msg); err != nil {
		return "", err
	}
	return "child session was idle, nothing to interrupt — message queued instead; call agent with resume_session_id to resume it", nil
}

// StopAgent implements tools.AgentControl (§4.3): batch Stop via Cancel
// (stopTree: the child's live work, its own roots, wake zeroing and
// auto-resume suspension) plus the driver-aware Cancel for the child's live
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
