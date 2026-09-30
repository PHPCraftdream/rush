package cmd

// Live work behind a released session lock (ASYNC-02). In phase 4 the session
// lock is held only during a turn and its file is truncated, not deleted, on
// release; a `rush run` loop waiting between turns (a running job, a
// delegation, a paced retry) therefore leaves an empty, aging lock file, the
// last turn's ended_reason and a finished last assistant message. The durable
// facts that the session is still worked on are the driver marker and the
// running job / delegation rows: `sessions watch|tail|locks|list|why` read
// them instead of treating "no live lock" as "ended" or "crashed".

import (
	"context"
	"os"
	"sort"
	"strings"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
)

// sessionLiveWork is what keeps a session open without a live lock.
type sessionLiveWork struct {
	driver      *session.SessionDriver
	driverOwes  bool
	ownJobs     []session.LiveJob
	descendants []session.LiveJob
	// unreadable lists reads that failed: unknown counts as live (the
	// codebase's liveness convention), so a hiccup never ends a watch.
	unreadable []string
}

func (w sessionLiveWork) active() bool {
	return w.driver != nil || len(w.ownJobs) > 0 || len(w.descendants) > 0 || len(w.unreadable) > 0
}

// describe renders the clauses naming the live work, "; "-separated.
func (w sessionLiveWork) describe() string {
	var parts []string
	if w.driver != nil {
		parts = append(parts, describeRunDriver(*w.driver, w.driverOwes))
	}
	if len(w.ownJobs) > 0 {
		parts = append(parts, describeLiveOwnJobs(w.ownJobs))
	}
	if len(w.descendants) > 0 {
		parts = append(parts, describeLiveDescendants(w.descendants))
	}
	for _, u := range w.unreadable {
		parts = append(parts, "could not read "+u+" (assuming it is live)")
	}
	return strings.Join(parts, "; ")
}

// inspectSessionLiveWork reads the driver marker (ONE statement), the
// session's own running jobs and its live delegations. An App without an
// AsyncJobStore answers no live work.
func inspectSessionLiveWork(ctx context.Context, a *app.App, sessionID string) sessionLiveWork {
	var w sessionLiveWork
	if a == nil {
		return w
	}
	if drivers, err := a.LiveSessionDrivers(ctx); err != nil {
		w.unreadable = append(w.unreadable, "the driver marker")
	} else if d, ok := drivers[sessionID]; ok {
		w.driver = &d
		if store := a.AsyncJobStore(); store != nil {
			w.driverOwes, _ = store.ReactionDebtExists(ctx, sessionID)
		}
	}
	if store := a.AsyncJobStore(); store != nil {
		var own, desc bool
		w.ownJobs, own = store.LiveOwnJobs(ctx, sessionID)
		w.descendants, desc = store.LiveDescendantJobs(ctx, sessionID)
		if own && len(w.ownJobs) == 0 {
			w.unreadable = append(w.unreadable, "the session's own jobs")
		}
		if desc && len(w.descendants) == 0 {
			w.unreadable = append(w.unreadable, "the session's delegations")
		}
	}
	return w
}

// lockIsCleanRelease reports whether sessionID's lock file is the leftover of
// a clean release (an existing file with no recorded PID) or names the live
// driver itself: the shape a loop leaves between turns, never a crash. A
// missing file is not one, and neither is a recorded dead PID of another
// process (that process crashed). driverPID is 0 when no driver is known.
func lockIsCleanRelease(dataDir, sessionID string, driverPID int) bool {
	path := session.SessionLockPath(dataDir, sessionID)
	if _, err := os.Stat(path); err != nil {
		return false
	}
	pid := session.ReadLockPID(path)
	return pid <= 0 || (driverPID > 0 && pid == driverPID)
}

// promoteCleanReleaseCrashes rescues sessions the lock classification called
// "crashed" although their lock is a clean release and live work exists (R6C-2:
// a paced retry after a failed reaction turn leaves an empty back-dated lock
// and an error finish). Such a session is treated like a finished one for the
// promotion layers -- driver, own job -> "running", delegation ->
// "delegating" -- and keeps "crashed" when none applies, when the lock records
// a dead PID that is not the driver's, or when the lock file is missing.
func promoteCleanReleaseCrashes(
	ctx context.Context,
	a *app.App,
	dataDir string,
	sessions []session.Session,
	statusByID map[string]string,
) map[string]string {
	if a == nil {
		return statusByID
	}
	var drivers map[string]session.SessionDriver
	var candidates []session.Session
	scratch := map[string]string{}
	for _, s := range sessions {
		if statusByID[s.ID] != "crashed" {
			continue
		}
		if drivers == nil {
			drivers, _ = a.LiveSessionDrivers(ctx)
			if drivers == nil {
				drivers = map[string]session.SessionDriver{}
			}
		}
		if !lockIsCleanRelease(dataDir, s.ID, int(drivers[s.ID].PID)) {
			continue
		}
		candidates = append(candidates, s)
		scratch[s.ID] = "done"
	}
	if len(candidates) == 0 {
		return statusByID
	}
	scratch = markDelegatingLiveDescendants(ctx, a, candidates, scratch)
	scratch = markRunningOwnJobs(ctx, a, candidates, scratch)
	scratch = markLiveRunDrivers(ctx, a, candidates, scratch)
	for _, s := range candidates {
		if promoted := scratch[s.ID]; promoted != "done" {
			statusByID[s.ID] = promoted
		}
	}
	return statusByID
}

// driversByLockName indexes the live loop markers by the lock-file stem
// (sanitiseSessionIDForFilename of the real id): a lock file name cannot be
// mapped back to the real id, so `sessions locks` matches by the stem (R7C-2).
// Two real ids that sanitise equally ("a/b", "a b") share one lock file; the
// candidates are kept in PID order so the choice is deterministic.
func driversByLockName(drivers map[string]session.SessionDriver) map[string][]session.SessionDriver {
	byName := make(map[string][]session.SessionDriver, len(drivers))
	for _, d := range drivers {
		stem := sanitiseSessionIDForFilename(d.SessionID)
		byName[stem] = append(byName[stem], d)
	}
	for _, list := range byName {
		sort.Slice(list, func(i, j int) bool {
			if list[i].PID != list[j].PID {
				return list[i].PID < list[j].PID
			}
			return list[i].SessionID < list[j].SessionID
		})
	}
	return byName
}

// lockDriver picks the live loop a released lock file belongs to: an empty
// file (pid <= 0) is the clean-release leftover of any candidate; a recorded
// PID belongs only to the candidate running under that PID. A colliding dead
// session's own recorded PID therefore never reads as a live loop's lock.
func lockDriver(candidates []session.SessionDriver, pid int) (session.SessionDriver, bool) {
	for _, d := range candidates {
		if pid <= 0 || int64(pid) == d.PID {
			return d, true
		}
	}
	return session.SessionDriver{}, false
}
