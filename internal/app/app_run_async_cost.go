package app

import (
	"context"
	"log/slog"
	"slices"
)

// cliChildCostWalkDepth bounds the delegation-tree walk of chargeRunningChildren
// (the session store's own walk is bounded at 16 too).
const cliChildCostWalkDepth = 16

// chargeRunningChildren moves the spend so far of every delegation still
// running below the run's root onto its parent, deepest first, so the cost
// window applyTotals reads next includes it (R5C-3). A child's spend reaches its
// parent only when its run returns; on a Ctrl-C, --timeout or cap exit the child
// is still running (CLI jobs outlive the turn's ctx) and its run returns only in
// App.Shutdown, after the envelope is flushed.
//
// Decision: charge before the window (rather than adding a recursive
// child.cost - parent_cost_accounted to the reading) because the delta already
// lives in one place: TransferChildCostToParent advances parent_cost_accounted
// in the same transaction, so the child's own later charge (its run returning,
// or Shutdown) moves only what accrued after this one -- the parent gets each
// dollar once, and every reader of the session's cost (`sessions cost`, the
// caps) sees the same number the envelope reports. Best effort: a failed read
// or transfer only leaves that delta for the child's own charge.
func (l *cliLoop) chargeRunningChildren() {
	if l.app == nil || l.app.asyncJobStore == nil || l.app.Sessions == nil || l.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	type edge struct {
		parent, child string
		depth         int
	}
	var edges []edge
	visited := map[string]struct{}{l.sessionID: {}}
	frontier := []string{l.sessionID}
	for depth := 0; depth < cliChildCostWalkDepth && len(frontier) > 0; depth++ {
		rows, err := l.app.asyncJobStore.ListRunningAsyncJobsForOwners(ctx, frontier)
		if err != nil {
			slog.Warn("rush run: could not read running delegations for the cost window", "session_id", l.sessionID, "err", err)
			break
		}
		frontier = nil
		for _, row := range rows {
			child := row.ChildSessionID.String
			if !row.ChildSessionID.Valid || child == "" {
				continue
			}
			edges = append(edges, edge{parent: row.OwnerSessionID, child: child, depth: depth})
			if _, seen := visited[child]; !seen {
				visited[child] = struct{}{}
				frontier = append(frontier, child)
			}
		}
	}
	slices.SortStableFunc(edges, func(a, b edge) int { return b.depth - a.depth })
	for _, e := range edges {
		if err := l.app.Sessions.TransferChildCostToParent(ctx, e.child, e.parent); err != nil {
			slog.Warn("rush run: could not charge a running child's spend to its parent", "child", e.child, "parent", e.parent, "err", err)
		}
	}
}
