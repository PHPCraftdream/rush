package session

// Cross-process descendant-work liveness: the durable-state layer beneath
// the coordinator's in-process parked-delegation registry (see
// internal/agent/subagent_outcome.go).
//
// Why it exists: every STATUS surface (`sessions list`, `sessions why`, the
// web session list) runs in a process that has no access to the in-memory
// registries the coordinator keeps — which async job is running, which
// background shell is live, which delegation is parked. A session that
// delegates work to a sub-agent releases its OWN lock the moment it
// yields, so a surface looking only at the session's own lock reports
// "done" / "at rest" for the whole duration of the delegation. That was
// observed in production: `rush sessions why` classified a root session as
// done because its per-turn lock was gone, while an implementation
// sub-agent session held a LIVE lock and the outer `rush run` was still
// alive waiting on it.
//
// What DOES cross the process boundary is the durable state: the child
// session rows (linked through parent_session_id) and the children's own
// session locks. LiveDescendants walks exactly that. It is the one helper
// every status surface shares, so none of them grows a second, divergent
// descendant classifier.

import (
	"context"
)

// SubSessionLister is the minimal read surface the descendant walk needs:
// the direct children of one session, via the DB's parent_session_id
// linkage. Satisfied by Service (ListSubSessions); declared separately so
// tests can supply a fake without a whole DB, and so the walk itself stays
// independent of the rest of the session package's surface.
type SubSessionLister interface {
	ListSubSessions(ctx context.Context, parentSessionID string) ([]Session, error)
}

// LiveDescendant names one descendant session whose lock is currently held
// and live — i.e. delegated work that is still in progress, possibly in a
// process this one cannot see.
type LiveDescendant struct {
	// ID is the descendant session's ID.
	ID string
	// Depth is 1 for a direct child, 2 for a grandchild, and so on. It is
	// how far below the queried root the live lock was found.
	Depth int
	// Lock is the observed lock state. Lock.Live is always true unless the
	// lock file could not be inspected at all (StatErr != nil), which the
	// walk deliberately treats as possibly-live.
	Lock LockState
}

// maxDescendantWalkDepth bounds the parent→child walk. The real call tree
// is a handful of levels deep (session → sub-agent → sub-sub-agent), so
// this only ever trips on corrupt linkage; it exists so a pathological
// cycle can never turn a status query into an unbounded walk.
const maxDescendantWalkDepth = 16

// LiveDescendants returns every descendant of sessionID — at any depth —
// whose session lock is currently held and live, plus a flag reporting
// whether the walk was able to enumerate the whole tree.
//
// The walk is breadth-first over the parent_session_id linkage and is both
// depth-bounded (maxDescendantWalkDepth) and cycle-guarded (a visited set),
// so corrupt or cyclic linkage data terminates instead of looping.
//
// Liveness per descendant is session.InspectSessionLock with the package's
// own LockStaleDuration as the heartbeat threshold: a fresh mtime is the
// fast path, and a stale mtime falls back to a real PID-liveness probe,
// itself bounded by maxPidFallbackAge — exactly the rules every other
// cross-process lock consumer in this codebase already uses, so "live"
// means the same thing here as it does on the web server's ownership
// annotation and in `sessions watch`. A descendant whose lock could not be
// inspected at all (StatErr) is reported as live: a status surface must
// not answer "done" for work it failed to verify.
//
// walkIncomplete is true when any level's child listing failed, meaning
// the returned slice may be missing live descendants. Callers that must
// not lie (e.g. `sessions why`'s verdict) surface that uncertainty;
// best-effort display callers (`sessions list`, the web list) may ignore
// it and let the next poll self-correct.
//
// Cost: one indexed child listing per visited node (the
// parent_session_id/created_at composite index serves the query), so a
// typical session costs a single lookup.
func LiveDescendants(
	ctx context.Context,
	lister SubSessionLister,
	dataDir, sessionID string,
) (live []LiveDescendant, walkIncomplete bool) {
	if lister == nil || sessionID == "" || dataDir == "" {
		return nil, false
	}
	visited := map[string]struct{}{sessionID: {}}
	frontier := []string{sessionID}
	for depth := 1; depth <= maxDescendantWalkDepth && len(frontier) > 0; depth++ {
		if ctx != nil && ctx.Err() != nil {
			return live, true
		}
		var next []string
		for _, parent := range frontier {
			children, err := lister.ListSubSessions(ctx, parent)
			if err != nil {
				// The tree below this parent is unknown, so a short result
				// means "not fully enumerated", never "no live descendants".
				walkIncomplete = true
				continue
			}
			for _, child := range children {
				if child.ID == "" {
					continue
				}
				if _, seen := visited[child.ID]; seen {
					// Cycle guard: linkage data that points back at an
					// already-visited session must not loop the walk.
					continue
				}
				visited[child.ID] = struct{}{}
				st := InspectSessionLock(dataDir, child.ID, LockStaleDuration)
				if st.Live || st.StatErr != nil {
					live = append(live, LiveDescendant{
						ID:    child.ID,
						Depth: depth,
						Lock:  st,
					})
				}
				next = append(next, child.ID)
			}
		}
		frontier = next
	}
	return live, walkIncomplete
}
