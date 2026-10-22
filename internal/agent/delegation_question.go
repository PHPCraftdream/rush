// The "delegation waits for an answer" state (#1157, A30), batch edition: a
// delegated child that called ask_question while its delegation is still held
// is visible to the parent through the batch held-question facts
// (BatchCutoverFacts.LiveHeldQuestions, the fenced current-incarnation rows)
// and answers through the batch held-answer bridge
// (deliverBatchHeldAnswer, coordinator_agent_inject_batch.go). This file keeps
// the supervision/inspect surface: the per-delegation AWAITING-YOUR-ANSWER
// summary line, derived from history plus batch facts.
package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// childQuestionMaxLen caps the quoted question in the supervision line.
const childQuestionMaxLen = 300

// childAwaitingQuestionSummary is supervision's per-delegation line for a
// delegation whose child is paused on a question. ok=false when the child is
// not question-paused (busy, never asked, or its last finished turn did not
// end on a question stop). The delegation's identity is the batch delegation
// node that names childID (its tool call id, read from the parent's live
// delegation facts); the running-own-work count is the child's own live batch
// roots.
func (c *coordinator) childAwaitingQuestionSummary(childID string, elapsed time.Duration) (line string, ok bool) {
	if c == nil || childID == "" {
		return "", false
	}
	if ag := c.agentFor(childID); ag != nil && ag.IsSessionBusy(childID) {
		return "", false
	}
	msg, awaiting := c.childLastActivity(context.Background(), childID)
	if !awaiting {
		return "", false
	}
	question := ""
	if msg != nil {
		if block, is := subAgentQuestionBlockFromFinish(msg.FinishPart()); is {
			question = block
		}
	}
	if question == "" {
		return "", false
	}
	if len(question) > childQuestionMaxLen {
		question = strings.TrimSpace(question[:childQuestionMaxLen]) + "..."
	}
	delegation := childID
	ownJobs := 0
	facts := coordinatorBatchCutoverFacts(c)
	if parent := c.childParentSession(childID); parent != "" {
		if nodes, err := facts.LiveDelegationNodes(context.Background(), parent); err == nil {
			for _, node := range nodes {
				if node.ChildSessionID == childID {
					if node.ToolCallID != "" {
						delegation = node.ToolCallID
					}
					break
				}
			}
		}
	}
	if own, _, err := facts.LiveWorkSummary(context.Background(), childID); err == nil {
		ownJobs = own
	}
	return fmt.Sprintf(
		"- %s (sub-agent, child session %s): AWAITING YOUR ANSWER for %s -- it asked: %s. Its own %d task(s) still run. Answer: agent(resume_session_id=%q, prompt=\"...\"); give up: stop_agent(child_session_id=%q).",
		delegation, childID, elapsed.Round(time.Second), question, ownJobs, childID, childID,
	), true
}

// childParentSession resolves childID's parent session id ("" when the
// session is unreadable or has no parent).
func (c *coordinator) childParentSession(childID string) string {
	if c == nil || c.sessions == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := c.sessions.Get(ctx, childID)
	if err != nil {
		return ""
	}
	return sess.ParentSessionID
}
