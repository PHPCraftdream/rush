package agent

import (
	"context"
	"log/slog"
)

// ownWorkBatch is the in-turn guard's flag-ON own-work predicate. Under the
// batch runtime the ON turn never touches async_jobs (agent_turn.go), so the
// legacy ledger's map is empty by construction and running() would answer
// false while live batch work runs — armProgressGuard must answer the same
// "does this owner have live work" question CLIActiveWork answers, through
// the same bound cutover-facts seam.
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
	if a.asyncJobs == nil || a.asyncJobs.coord == nil {
		return false
	}
	coord := a.asyncJobs.coord
	if !batchOwnWorkFactsInstalled(coord) {
		// No batch runtime installed for this coordinator (cli_active_work.go's
		// unbound rule): the caller keeps the legacy ledger expression, and the
		// helper itself never answers from batch rows.
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
// true only when the batch cutover facts are bound and installed for coord.
// It is the same rule CLIActiveWork's ON branch uses (cli_active_work.go) — a
// flag-ON coordinator without an installed runtime keeps its legacy path
// there, so the guard must keep the legacy ledger expression too and never
// hide live legacy work behind an unbound helper.
func batchOwnWorkFactsInstalled(coord *coordinator) bool {
	if coord == nil {
		return false
	}
	facts := coordinatorBatchCutoverFacts(coord)
	if facts == nil {
		return false
	}
	_, unbound := facts.(unknownBatchCutoverFacts)
	return !unbound
}
