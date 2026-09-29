// Cross-process durable-state reader for descendant-work liveness (phase-4
// step 7, docs/plans/2026-09-28-async-phase4-durable-core.md sec.3.8:
// "Читатели между процессами"). Replaces descendant_liveness.go's
// session-lock heuristic: a session shows live work iff it owns a
// 'running' async_jobs row whose host is not provably dead (sec.3.6's
// 3-way probe, via the SHARED, non-acquiring ProbeHostShared -- a reader
// must never take the exclusive lock, which could race a recoverer), or
// one of its live delegations' children does. The walk follows
// child_session_id (a delegation row), NEVER parent_session_id -- doc
// sec.3.5 is explicit descendants are not walked that way.
package session

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// maxDescendantWalkDepth bounds the delegation-tree walk (moved from the
// removed descendant_liveness.go): the real call tree is a handful of
// levels deep in practice, so this only ever trips on corrupt/cyclic
// linkage data.
const maxDescendantWalkDepth = 16

// LiveJob names one concrete 'running' async_jobs row LiveJobs/
// LiveDescendantJobs found live: its host holds the OS lock
// (HostStatusAlive) or the probe could not tell (HostStatusUnknown, doc
// sec.3.6: never treated as dead). A row whose host is confirmed dead is
// never returned by either walk.
type LiveJob struct {
	SessionID      string // owner_session_id
	Depth          int    // 0 = root itself, 1 = direct delegation child, ...
	ToolCallID     string
	Kind           string
	HostID         string
	HostStatus     HostLockStatus // Alive or Unknown -- never Dead
	StartedAt      time.Time
	ChildSessionID string // "" unless this row is a delegation followed into
}

// HostLiveness is the reader's per-row liveness check (doc sec.3.6/3.8):
// this process's own host is alive by definition and is never probed
// (IsOwnHostID); anything else goes through the shared, non-acquiring probe
// (ProbeHostShared) so a live recoverer holding the exclusive lock is never
// disturbed. A probe error that isn't a definite HostStatusDead verdict is
// folded into HostStatusUnknown -- doc sec.3.6: any outcome other than a
// clean "dead" leaves the row visible, never silently reapable.
func (s *AsyncJobStore) HostLiveness(hostID string) HostLockStatus {
	if hostID == "" {
		return HostStatusUnknown
	}
	if IsOwnHostID(hostID) {
		return HostStatusAlive
	}
	status, err := ProbeHostShared(s.dataDir, hostID)
	if err != nil && status != HostStatusDead {
		return HostStatusUnknown
	}
	return status
}

// ListRunningAsyncJobsForOwners exposes the query LiveJobs walks (doc sec.5
// step 7): every 'running' row owned by one of ownerIDs, batched per BFS
// level rather than one call per session.
func (s *AsyncJobStore) ListRunningAsyncJobsForOwners(ctx context.Context, ownerIDs []string) ([]db.AsyncJob, error) {
	return s.q.ListRunningAsyncJobsForOwners(ctx, ownerIDs)
}

// ListAsyncJobsForOwner exposes the query `sessions jobs`/`sessions why`
// need: every async_jobs row (any state) owned by one session, oldest
// first.
func (s *AsyncJobStore) ListAsyncJobsForOwner(ctx context.Context, owner string) ([]db.AsyncJob, error) {
	return s.q.ListAsyncJobsForOwner(ctx, owner)
}

// GetAsyncHost exposes the display-only async_hosts row for `sessions
// jobs`'s host/pid/label column (doc sec.3.6: async_hosts is display-only
// bookkeeping, never consulted for liveness).
func (s *AsyncJobStore) GetAsyncHost(ctx context.Context, id string) (db.AsyncHost, error) {
	return s.q.GetAsyncHost(ctx, id)
}

// LiveJobs returns every 'running' async_jobs row live at or below
// rootSessionID -- root itself INCLUDED at depth 0 -- following delegation
// rows (child_session_id) only from rows this reader itself judged live: a
// delegation row on a confirmed-dead host is dropped and its child is never
// added to the walk (doc sec.3.8: "живые делегации, чьи дети имеют живую
// работу").
//
// walkIncomplete is true if any level's query failed -- the returned slice
// may then be missing live rows, never the reverse (a short result must
// never be read as "nothing is running").
func (s *AsyncJobStore) LiveJobs(ctx context.Context, rootSessionID string) (live []LiveJob, walkIncomplete bool) {
	if s == nil || rootSessionID == "" {
		return nil, false
	}
	visited := map[string]struct{}{rootSessionID: {}}
	frontier := []string{rootSessionID}
	for depth := 0; depth <= maxDescendantWalkDepth && len(frontier) > 0; depth++ {
		if ctx != nil && ctx.Err() != nil {
			return live, true
		}
		rows, err := s.q.ListRunningAsyncJobsForOwners(ctx, frontier)
		if err != nil {
			// The durable state for this level is unknown -- a short result
			// means "not fully enumerated", never "nothing running".
			walkIncomplete = true
			frontier = nil
			continue
		}
		var next []string
		for _, row := range rows {
			status := s.HostLiveness(row.HostID)
			if status == HostStatusDead {
				continue
			}
			job := LiveJob{
				SessionID:  row.OwnerSessionID,
				Depth:      depth,
				ToolCallID: row.ToolCallID,
				Kind:       row.Kind,
				HostID:     row.HostID,
				HostStatus: status,
				StartedAt:  time.Unix(row.CreatedAt, 0),
			}
			if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
				child := row.ChildSessionID.String
				if _, seen := visited[child]; !seen {
					visited[child] = struct{}{}
					next = append(next, child)
					job.ChildSessionID = child
				}
			}
			live = append(live, job)
		}
		frontier = next
	}
	return live, walkIncomplete
}

// LiveDescendantJobs is LiveJobs filtered to DELEGATION rows only (non-empty
// ChildSessionID) -- the direct replacement for the removed
// descendant_liveness.go's LiveDescendants: "does some DESCENDANT session
// still have live work".
//
// Filtering on ChildSessionID rather than Depth==0 matters: a single-level
// delegation's evidence IS the root's own row (owner=root, Depth=0,
// ChildSessionID=child) -- root itself claimed that job, so excluding every
// Depth==0 entry would exclude the ONLY evidence a fresh (not yet
// self-delegating) child ever produces. What must never appear is the
// root's own id AS A DESCENDANT, so each qualifying entry's
// ChildSessionID -- the actual named descendant, doc sec.5 step 7's
// "LiveDescendantIDs-style results" -- is checked against rootSessionID
// directly (belt-and-suspenders: a delegation row can never legitimately
// name its own root, but a corrupt/cyclic chain must not surface it either
// way).
func (s *AsyncJobStore) LiveDescendantJobs(ctx context.Context, rootSessionID string) (live []LiveJob, walkIncomplete bool) {
	all, walkIncomplete := s.LiveJobs(ctx, rootSessionID)
	for _, j := range all {
		if j.ChildSessionID == "" || j.ChildSessionID == rootSessionID {
			continue
		}
		live = append(live, j)
	}
	return live, walkIncomplete
}

// JobsInTree lists every async_jobs row (ANY state, not just 'running') at
// or below rootSessionID -- root's own rows included at depth 0 -- for
// `sessions jobs`'s full observation view. Unlike LiveJobs, every
// delegation link is followed regardless of the owning row's state or host
// liveness: a finished delegation's child sub-tree is still part of the
// history `sessions jobs` shows.
func (s *AsyncJobStore) JobsInTree(ctx context.Context, rootSessionID string) (jobs []db.AsyncJob, walkIncomplete bool) {
	if s == nil || rootSessionID == "" {
		return nil, false
	}
	visited := map[string]struct{}{rootSessionID: {}}
	queue := []string{rootSessionID}
	for depth := 0; depth <= maxDescendantWalkDepth && len(queue) > 0; depth++ {
		if ctx != nil && ctx.Err() != nil {
			return jobs, true
		}
		var next []string
		for _, owner := range queue {
			rows, err := s.q.ListAsyncJobsForOwner(ctx, owner)
			if err != nil {
				walkIncomplete = true
				continue
			}
			for _, row := range rows {
				jobs = append(jobs, row)
				if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
					child := row.ChildSessionID.String
					if _, seen := visited[child]; !seen {
						visited[child] = struct{}{}
						next = append(next, child)
					}
				}
			}
		}
		queue = next
	}
	return jobs, walkIncomplete
}
