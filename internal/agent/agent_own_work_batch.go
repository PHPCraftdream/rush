package agent

import (
	"context"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/clibridge"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// ownWorkBatch is the in-turn guard's own-work predicate on batch facts. The
// turn never touches a legacy job store (agent_turn.go), so the guard answers
// the same "does this owner have live work" question CLIActiveWork answers,
// through the same bound cutover-facts seam.
//
// The predicate is CLIActiveWork's flag-ON body WITHOUT
// recoverBatchScopeDeadCandidates: that recovery already ran at this turn's
// preamble, and armProgressGuard runs once per step, so repeating the
// dead-host sweep here would multiply bounded recovery writes per turn. A
// dead host the sweep would have recovered only keeps the conservative
// (active) answer until the next turn.
//
// A failed read fails closed exactly like CLIActiveWork's batch branch: the
// session is treated as having live work (ownWork=true), never as silently
// idle.
func (a *sessionAgent) ownWorkBatch(ctx context.Context, sessionID string) bool {
	if a.coord == nil {
		return false
	}
	coord := a.coord
	if !coord.BatchFactsInstalled() {
		// No batch runtime installed for this coordinator (cli_active_work.go's
		// unbound rule): the guard never answers from batch rows and the
		// caller treats the session as idle -- an unbound coordinator has no
		// durable live-work source to consult.
		return false
	}
	facts := coordinatorBatchCutoverFacts(coord)
	bctx, cancel := context.WithTimeout(ctx, drainAccountBudget())
	defer cancel()
	own, descendants, err := facts.LiveWorkSummary(bctx, sessionID)
	if err != nil {
		slog.Warn("Batch own-work read failed; treating the session as active",
			"session_id", sessionID, "err", err)
		return true
	}
	if own+descendants > 0 {
		active, err := clibridge.NewCliActiveWorkComponent(coord).DoBatchActiveAfterQuestionPause(bctx, facts, sessionID)
		if err != nil {
			slog.Warn("Batch own-work question-pause read failed; treating the session as active",
				"session_id", sessionID, "err", err)
			return true
		}
		if active {
			return true
		}
	}
	if coord.background != nil && (coord.background.ActiveOwned(sessionID) > 0 || coord.background.PendingCompletionsOwned(sessionID) > 0) {
		return true
	}
	return false
}

// CLIActiveWork excludes schedules, debt, and question-paused delegations, but
// includes those children's jobs and completion-persistence holds. Batch-only:
// the live scope comes from batch_nodes (the node-backed fact source bound at
// install). An UNBOUND coordinator (no bound facts) answers false: no batch
// runtime means no batch work can be pending for it. A failed read fails
// closed with the error (the quota loop retries it under its bounded window,
// never silently reads "idle").
func (c *coordinator) CLIActiveWork(ctx context.Context, root string) (bool, error) {
	return clibridge.NewCliActiveWorkComponent(c).DoCLIActiveWork(ctx, root)
}

var _ clibridge.CLIActiveWorkSource = (*coordinator)(nil)

func (c *coordinator) cliActiveWorkBatch(ctx context.Context, facts BatchCutoverFacts, root string) (bool, error) {
	return clibridge.NewCliActiveWorkComponent(c).DoCLIActiveWorkBatch(ctx, facts, root)
}

func (c *coordinator) BatchCutoverFacts() BatchCutoverFacts {
	return coordinatorBatchCutoverFacts(c)
}

func (c *coordinator) SubAgentQuestionBlockFromFinish(fp *message.Finish) (string, bool) {
	return subAgentQuestionBlockFromFinish(fp)
}

func (c *coordinator) SubAgentQuestionFromFinish(childSessionID string, fp *message.Finish) (string, bool) {
	return subAgentQuestionFromFinish(childSessionID, fp)
}

func (c *coordinator) BatchNodeStateLive(state string) bool {
	return batchNodeStateLive(state)
}

// BatchFactsInstalled reports whether a concrete (node-backed) batch cutover
// facts source is bound for the coordinator.
func (c *coordinator) BatchFactsInstalled() bool {
	if c == nil {
		return false
	}
	facts := coordinatorBatchCutoverFacts(c)
	return !c.UnboundBatchFacts(facts)
}

func (c *coordinator) UnboundBatchFacts(facts agentcall.BatchCutoverFacts) bool {
	_, unbound := facts.(unknownBatchCutoverFacts)
	return unbound
}

func (c *coordinator) RecoverBatchScopeDeadCandidates(ctx context.Context, owner string) {
	recoverBatchScopeDeadCandidates(ctx, c, owner)
}

func (c *coordinator) BackgroundOwnsWork(root string) bool {
	return c.background != nil && (c.background.ActiveOwned(root) > 0 || c.background.PendingCompletionsOwned(root) > 0)
}

func (c *coordinator) CLIActiveWorkMessages() message.Service { return c.messages }

func (c *coordinator) ChildSessionBusy(child string) bool {
	ag := c.agentFor(child)
	return ag != nil && ag.IsSessionBusy(child)
}
