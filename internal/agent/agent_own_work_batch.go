package agent

import (
	"context"
	"log/slog"
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
	if !batchOwnWorkFactsInstalled(coord) {
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
		active, err := coord.batchActiveAfterQuestionPause(bctx, facts, sessionID)
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

// batchOwnWorkFactsInstalled is the guard's "batch facts installed" predicate:
// true only when a concrete (node-backed) batch cutover facts source is bound
// for coord. It is the same rule CLIActiveWork uses (cli_active_work.go) — an
// unbound coordinator keeps the conservative idle answer and never hides live
// work behind an unbound helper.
func batchOwnWorkFactsInstalled(coord *coordinator) bool {
	if coord == nil {
		return false
	}
	facts := coordinatorBatchCutoverFacts(coord)
	_, unbound := facts.(unknownBatchCutoverFacts)
	return !unbound
}
