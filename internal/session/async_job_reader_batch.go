// Batched descendant-work reader for list views (R2A-12): the web session list
// asks, for EVERY session it shows, "does a descendant have live work" and
// "does the session itself have a live plain job" on each 5s re-poll. Asking
// per session (LiveDescendantJobs + LiveOwnJobs) is two or more reader queries
// and one host probe per session per poll; LiveWorkForRoots answers all of them
// from one shared walk: one query per BFS level over the UNION of all roots'
// frontiers (chunked to stay under SQLite's variable limit), and one liveness
// probe per distinct host per call.
package session

import (
	"context"
	"sort"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

// liveWorkQueryChunk bounds the owner ids of one IN (...) query: SQLite caps
// bound variables per statement (32766 in current builds, 999 in old ones).
const liveWorkQueryChunk = 500

// RootLiveWork is one root's answer from LiveWorkForRoots: exactly what
// LiveDescendantJobs(root) and LiveOwnJobs(root) return for that root alone.
type RootLiveWork struct {
	Descendants []LiveJob // == LiveDescendantJobs(root)
	Own         []LiveJob // == LiveOwnJobs(root)
	// DescendantsIncomplete/OwnIncomplete are the walkIncomplete flags of the
	// two single-root readers: a short result then never means "nothing
	// running".
	DescendantsIncomplete bool
	OwnIncomplete         bool
}

// LiveWorkForRoots computes LiveDescendantJobs and LiveOwnJobs for every root
// in one batched walk (see the file doc). Roots that are empty are skipped;
// every other root has an entry, empty when it has no live work. The reads run
// on the reader pool, and host liveness is decided once per distinct host per
// call, like LiveJobs.
func (s *AsyncJobStore) LiveWorkForRoots(ctx context.Context, roots []string) map[string]RootLiveWork {
	out := make(map[string]RootLiveWork, len(roots))
	if s == nil {
		return out
	}
	var frontier []string
	seenRoot := make(map[string]struct{}, len(roots))
	for _, r := range roots {
		if r == "" {
			continue
		}
		if _, dup := seenRoot[r]; dup {
			continue
		}
		seenRoot[r] = struct{}{}
		frontier = append(frontier, r)
	}
	if len(frontier) == 0 {
		return out
	}

	hostStatus := make(map[string]HostLockStatus)
	statusOf := func(hostID string) HostLockStatus {
		st, cached := hostStatus[hostID]
		if !cached {
			st = s.HostLiveness(hostID)
			hostStatus[hostID] = st
		}
		return st
	}

	// Shared fetch: every owner reached from any root, once, at its minimum
	// depth. Edges are followed only from rows whose host is not dead, exactly
	// like LiveJobs.
	running := make(map[string][]db.AsyncJob)
	failed := make(map[string]struct{}) // owners whose query failed or was cut off
	fetched := make(map[string]struct{})
	level := frontier
	for depth := 0; depth <= maxDescendantWalkDepth && len(level) > 0; depth++ {
		var todo []string
		for _, owner := range level {
			if _, done := fetched[owner]; !done {
				fetched[owner] = struct{}{}
				todo = append(todo, owner)
			}
		}
		var next []string
		for start := 0; start < len(todo); start += liveWorkQueryChunk {
			end := min(start+liveWorkQueryChunk, len(todo))
			chunk := todo[start:end]
			if ctx != nil && ctx.Err() != nil {
				for _, owner := range chunk {
					failed[owner] = struct{}{}
				}
				continue
			}
			rows, err := s.readQuerier().ListRunningAsyncJobsForOwners(ctx, chunk)
			if err != nil {
				for _, owner := range chunk {
					failed[owner] = struct{}{}
				}
				continue
			}
			for _, row := range rows {
				running[row.OwnerSessionID] = append(running[row.OwnerSessionID], row)
				if statusOf(row.HostID) == HostStatusDead {
					continue
				}
				if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
					next = append(next, row.ChildSessionID.String)
				}
			}
		}
		level = next
	}

	for root := range seenRoot {
		out[root] = liveWorkForRoot(root, running, failed, statusOf)
	}
	return out
}

// liveWorkForRoot replays LiveJobs/LiveDescendantJobs/LiveOwnJobs for one root
// over the rows LiveWorkForRoots already fetched.
func liveWorkForRoot(root string, running map[string][]db.AsyncJob, failed map[string]struct{}, statusOf func(string) HostLockStatus) RootLiveWork {
	var work RootLiveWork
	if _, bad := failed[root]; bad {
		work.OwnIncomplete = true
	}
	for _, row := range running[root] {
		if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
			continue
		}
		status := statusOf(row.HostID)
		if status == HostStatusDead {
			continue
		}
		work.Own = append(work.Own, liveJobOf(row, 0, status))
	}

	visited := map[string]struct{}{root: {}}
	frontier := []string{root}
	for depth := 0; depth <= maxDescendantWalkDepth && len(frontier) > 0; depth++ {
		// One level at a time, oldest first across the level, like LiveJobs.
		var level []db.AsyncJob
		for _, owner := range frontier {
			if _, bad := failed[owner]; bad {
				work.DescendantsIncomplete = true
				continue
			}
			level = append(level, running[owner]...)
		}
		sort.SliceStable(level, func(i, j int) bool { return level[i].CreatedAt < level[j].CreatedAt })
		var next []string
		for _, row := range level {
			status := statusOf(row.HostID)
			if status == HostStatusDead {
				continue
			}
			job := liveJobOf(row, depth, status)
			if row.ChildSessionID.Valid && row.ChildSessionID.String != "" {
				child := row.ChildSessionID.String
				if _, seen := visited[child]; !seen {
					visited[child] = struct{}{}
					next = append(next, child)
					job.ChildSessionID = child
				}
				if job.ChildSessionID != "" && job.ChildSessionID != root {
					work.Descendants = append(work.Descendants, job)
				}
			}
		}
		frontier = next
	}
	if len(frontier) > 0 {
		work.DescendantsIncomplete = true
	}
	return work
}

func liveJobOf(row db.AsyncJob, depth int, status HostLockStatus) LiveJob {
	return LiveJob{
		SessionID:  row.OwnerSessionID,
		Depth:      depth,
		ToolCallID: row.ToolCallID,
		Kind:       row.Kind,
		ToolName:   row.ToolName,
		HostID:     row.HostID,
		HostStatus: status,
		StartedAt:  time.Unix(row.CreatedAt, 0),
	}
}
