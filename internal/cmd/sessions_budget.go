// Run-cost display helpers for the sessions commands (#1130): the row's own
// ledger is OwnCost; "including children" is the subtree budget, computed at
// read time and degraded to the own ledger when the tree read fails.

package cmd

import (
	"context"
	"sort"

	"github.com/PHPCraftdream/rush/internal/session"
)

// sessionBudget returns the session's subtree budget (own + delegation
// children since the last reset); a failed tree read degrades to the own
// ledger rather than showing a wrong-looking zero.
func sessionBudget(ctx context.Context, svc session.Service, s session.Session) float64 {
	budget, err := svc.SubtreeBudget(ctx, s.ID)
	if err != nil {
		return s.OwnCost
	}
	return budget
}

// sessionBudgets maps every listed root to its subtree budget.
func sessionBudgets(ctx context.Context, svc session.Service, sessions []session.Session) map[string]float64 {
	budgets := make(map[string]float64, len(sessions))
	for _, s := range sessions {
		budgets[s.ID] = sessionBudget(ctx, svc, s)
	}
	return budgets
}

// orderBySubtreeActivity sorts newest-first by max(updated_at) over each
// session's delegation subtree (#1130): a busy child keeps its root at the
// top of `sessions list` without writing the parent's updated_at. A failed
// read falls back to the row's own updated_at.
func orderBySubtreeActivity(ctx context.Context, svc session.Service, sessions []session.Session) {
	active := make(map[string]int64, len(sessions))
	for _, s := range sessions {
		ts, err := svc.SubtreeUpdatedAt(ctx, s.ID)
		if err != nil {
			ts = s.UpdatedAt
		}
		active[s.ID] = ts
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		return active[sessions[i].ID] > active[sessions[j].ID]
	})
}
