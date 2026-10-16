// Flag-ON (RUSH_BATCH_RUNTIME) fact readers for the app layer's activity
// and quota surfaces (package R12-F): reaction debt, live work, host
// liveness and finished-work export read ONLY batch node state
// (batch_nodes and its debt predicates). The flag-OFF branches in
// app_session_activity.go and app_run_quota.go keep the AsyncJobStore
// readers byte-for-byte. Nothing here reads async_jobs. The readers build
// their own handles from the App's pools, so they work on an App with no
// batch runtime installed (no provider configured).

package app

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchWorkFacts is the runtime-neutral live-work shape both flag branches
// fill: the counts and the unreadable flags the classifier consumes.
type batchWorkFacts struct {
	Own, Descendants      int
	DescendantSessionIDs  []string
	OwnIncomplete         bool
	DescendantsIncomplete bool
}

// batchCutoverOn reads the flag at call time, exactly like the R11F
// install gate (app.go) and the other R12 consumers select behavior.
func batchCutoverOn() bool { return config.BatchRuntimeEnabled() }

// batchCutoverWriter is the pool the batch debt store opens its read-only
// transactions on (same pool choice as BatchCLIStore). Nil without a DB.
func (app *App) batchCutoverWriter() *sql.DB {
	if app == nil || app.DB == nil {
		return nil
	}
	writer := app.DB()
	if writer == nil {
		return nil
	}
	if len(app.dbConns) > 0 && app.dbConns[0] != nil {
		writer = app.dbConns[0]
	}
	return writer
}

// batchCutoverReader returns the read pool the batch live reader runs on
// (nil falls back to the writer, like BatchCLIStore).
func (app *App) batchCutoverReader(writer *sql.DB) *sql.DB {
	if app.readDB != nil {
		return app.readDB
	}
	return writer
}

// batchCutoverDebtOwners answers the pending-inclusive reaction-debt fact
// from batch node/outbox predicates only. A nil pool answers "no debt"
// (the configless-App rule), never an error.
func (app *App) batchCutoverDebtOwners(ctx context.Context, ids []string) (map[string]bool, error) {
	writer := app.batchCutoverWriter()
	if writer == nil {
		return map[string]bool{}, nil
	}
	return session.NewBatchStore(writer).BatchReactionDebtOwners(ctx, ids)
}

// batchCutoverLiveWork derives the live own-work and live-delegation facts
// from batch nodes: BatchLiveForSessions walks the owner's live roots and,
// delegation links included, the child-session forest in one shared BFS.
// Host death never settles nodes (dead hosts stay live until recovery
// commits), so there is no dead-host-running-rows fact under the batch
// authority; a failed or Unknown-host read stays incomplete and surfaces
// as an Unreadable entry, never as "nothing running".
func (app *App) batchCutoverLiveWork(ctx context.Context, ids []string) map[string]batchWorkFacts {
	out := make(map[string]batchWorkFacts, len(ids))
	writer := app.batchCutoverWriter()
	if writer == nil {
		return out
	}
	reader := session.NewBatchLiveReader(db.New(app.batchCutoverReader(writer)), app.dataDir)
	for id, r := range reader.BatchLiveForSessions(ctx, ids) {
		f := batchWorkFacts{
			Own:                   len(r.Own),
			Descendants:           len(r.Descendants),
			OwnIncomplete:         !r.OwnComplete,
			DescendantsIncomplete: !r.Complete,
		}
		seen := make(map[string]struct{}, len(r.Descendants))
		for _, n := range r.Descendants {
			child := n.ChildSessionID
			if child == "" {
				continue
			}
			if _, dup := seen[child]; dup {
				continue
			}
			seen[child] = struct{}{}
			f.DescendantSessionIDs = append(f.DescendantSessionIDs, child)
		}
		out[id] = f
	}
	return out
}

// batchCutoverHostLiveness decides host liveness for the flag-ON driver
// facts with the shared, non-acquiring probe (the same predicate
// BatchLiveReader uses), never through the AsyncJobStore.
func (app *App) batchCutoverHostLiveness() func(string) session.HostLockStatus {
	return func(hostID string) session.HostLockStatus {
		if hostID == "" || app.dataDir == "" {
			return session.HostStatusUnknown
		}
		st, err := session.ProbeHostShared(app.dataDir, hostID)
		if err != nil {
			return session.HostStatusUnknown
		}
		if st != session.HostStatusAlive && st != session.HostStatusDead {
			return session.HostStatusUnknown
		}
		return st
	}
}

// quotaWorkRow is the runtime-neutral finished-work row both flag branches
// fill. Key is the per-node identity the quota terminal snapshot dedupes
// on (the claim id on the legacy path, the root/node incarnation on the
// batch path); Announced mirrors the legacy announced flag, batch rows
// inheriting the ROOT row's announcement (the tree's delivery is announced
// by the root).
type quotaWorkRow struct {
	OwnerSessionID string
	Key            string
	ToolCallID     string
	State          string
	Announced      bool
	Result         string
	ChildSessionID string
}

// batchFinishedWork exports every row of the session's batch tree
// (owner roots and, via child_session_id, the descendants' roots --
// terminal roots INCLUDED, which is why the enumeration reads
// BatchHistoryRootsForOwner, not the live-roots query). Errors report
// incomplete, never a partial silent answer.
func (app *App) batchFinishedWork(ctx context.Context, sessionID string) ([]quotaWorkRow, bool, error) {
	conn := app.activityConn()
	if conn == nil {
		return nil, false, nil
	}
	visited := map[string]bool{sessionID: true}
	frontier := []string{sessionID}
	var out []quotaWorkRow
	for depth := 0; depth <= batchReadMaxDepth && len(frontier) > 0; depth++ {
		var next []string
		for _, owner := range frontier {
			roots, err := conn.BatchHistoryRootsForOwner(ctx, owner)
			if err != nil {
				return nil, true, fmt.Errorf("quota exit: batch finished work: roots: %w", err)
			}
			for _, root := range roots {
				nodes, err := conn.BatchLevelNodes(ctx, db.BatchLevelNodesParams{Owner: root.OwnerSessionID, RootID: root.RootID})
				if err != nil {
					return nil, true, fmt.Errorf("quota exit: batch finished work: nodes: %w", err)
				}
				announced := root.Announced != 0
				for _, n := range nodes {
					child := n.ChildSessionID.String
					if n.ChildSessionID.Valid && child != "" && !visited[child] {
						visited[child] = true
						next = append(next, child)
					}
					jobID := n.NodeID
					if n.NodeID == n.RootID && root.ToolCallID.Valid && root.ToolCallID.String != "" {
						jobID = root.ToolCallID.String
					}
					out = append(out, quotaWorkRow{
						OwnerSessionID: n.OwnerSessionID,
						Key:            n.RootID + "/" + n.NodeID,
						ToolCallID:     jobID,
						State:          n.State,
						Announced:      announced,
						Result:         n.OutcomeSummary,
						ChildSessionID: child,
					})
				}
			}
		}
		frontier = next
	}
	// A depth-bounded walk must never read as complete (same rule as the
	// session live-work walk).
	if len(frontier) > 0 {
		return out, true, nil
	}
	return out, false, nil
}

// batchReadMaxDepth matches the session-side batch walk depth bound.
const batchReadMaxDepth = 16
