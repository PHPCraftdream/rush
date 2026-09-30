// Coordinator-side helpers for the work ledger: the delegation completion
// refresh, the child-run-ended re-check trigger, and the parked-delegation
// status reporter. Moved out of subagent_outcome.go (deleted by phase 1,
// docs/plans/2026-09-27-async-phase1-spec.md) unchanged in behavior -- these
// stay coordinator methods (refreshSubAgentCompletion needs c.messages)
// rather than moving onto workLedger itself.
//
// Phase 3 (docs/plans/2026-09-28-async-phase3-spec.md §3) deleted
// DescendantWorkPending/anyPendingWorkInMemory/sessionOwnsPendingWork. Phase 4
// replaced their successor workLedger.next() with the DB scope predicate
// (coordinator.CLIScope) -- see app_run_async.go's
// runNonInteractiveWithAsyncResults and docs/async-invariants.md's ASYNC-02
// row.
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
//
// Doc sec.3.4: a child session whose reaction debt was closed by failure
// (settle-by-failure, coordinator_drain_policy.go) gets no further turn, so
// its last finished assistant message would be stale or nonexistent. Such a
// child finishes its delegation as FAILED, with the text of its
// settle-closed (reacted_failed=1) notices instead of that stale message --
// checked FIRST, before the normal last-message read.
func (c *coordinator) refreshSubAgentCompletion(childSessionID string, completion AsyncCompletion) AsyncCompletion {
	if c.asyncJobs != nil && c.asyncJobs.store != nil {
		failCtx, failCancel := context.WithTimeout(context.Background(), 5*time.Second)
		failedTexts, err := c.asyncJobs.store.ListReactedFailedText(failCtx, childSessionID)
		failCancel()
		if err != nil {
			slog.Debug("sub-agent outcome refresh: reacted-failed check failed, continuing with normal read",
				"child_session", childSessionID, "err", err)
		} else if len(failedTexts) > 0 {
			lines := make([]string, 0, len(failedTexts))
			for _, ft := range failedTexts {
				lines = append(lines, ft.Text)
			}
			completion.Content = tools.TruncateOutput(strings.Join(lines, "\n\n"))
			completion.IsError = true
			return completion
		}
	}
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
		} else if question, ok := subAgentQuestionFromFinish(childSessionID, msg.FinishPart()); ok {
			// The child asked a question in a Drain turn: the parent gets the
			// question (as a paused sub-agent, not a failure), like the
			// first-turn path of runSubAgent does.
			completion.Content = tools.TruncateOutput(question)
			completion.IsError = false
			return completion
		} else {
			// Doc sec.3.8 ("Итог делегации"): the child's last finished
			// message has no text at all (only reasoning and/or tool
			// calls) -- the parent must NOT see the stale text captured
			// when the delegation was parked (arm time); it gets this
			// fixed notice instead.
			completion.Content = subAgentNoFinalTextText
		}
		completion.IsError = msg.FinishReason() == message.FinishReasonError
		return completion
	}
	return completion
}

// subAgentNoFinalTextText is doc sec.3.8's fixed wording for a delegation
// whose child's last finished turn produced no text content (only
// reasoning/tool calls) -- see refreshSubAgentCompletion's doc.
const subAgentNoFinalTextText = "завершено без итогового ответа"

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
