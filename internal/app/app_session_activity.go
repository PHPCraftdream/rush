// The session activity reader (plan R-ACT): collects the ActivityFacts the
// pure classifier (internal/session's ClassifySessionActivity) reduces to a
// verdict, for one session and -- for list views -- for many in one batched
// pass: one query per fact kind (driver markers, reaction debt, live work,
// wake schedules, ended reasons), never N, every predicate living in ONE
// sqlc query (architect decision 13: no second SQL formulation). This is
// the single place readers (internal/cmd, internal/server) will go through
// in step 2; the layers they layer today (inspectSessionLiveWork/promote*/
// mark*/reclassify*) are replaced by it.
//
// The single-session reader is the batch reader with one id, so the two can
// never disagree. A failed read of a LIVE-WORK fact is recorded in
// Facts.Unreadable and keeps the session open (the fail-open convention);
// a failed read of an END fact is recorded in Facts.EndUnreadable and never
// confers liveness (decision 7).
//
// Step 3 note (decision 12): the web flags are derived from the FACTS, not
// from the verdict -- Facts.Driver != nil || Facts.OwnRunningJobs > 0 -->
// HasLiveOwnWork, Facts.LiveDelegations > 0 --> HasLiveDescendantWork.
// SessionActivity carries the Facts for exactly that; do not switch the
// flags over to Kind.

package app

import (
	"context"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// SessionActivity pairs one session's facts with their verdict.
type SessionActivity struct {
	Facts   session.ActivityFacts
	Verdict session.ActivityVerdict
}

// SessionActivityBatchResult is the batch reader's answer: the per-id
// verdicts plus the lock-file-stem reverse map (decision 11): several real
// ids sanitise into one lock-file name (R7C-2/R8C-8), and locks/reap/prune
// need stem -> real ids to never rebuild their own mapping.
type SessionActivityBatchResult struct {
	ByID map[string]SessionActivity
	// LockStems maps session.SessionLockStem(id) to the requested ids that
	// sanitise to it.
	LockStems map[string][]string
}

// SessionActivity classifies one session.
func (app *App) SessionActivity(ctx context.Context, sessionID string) (SessionActivity, error) {
	out, err := app.SessionActivityBatch(ctx, []string{sessionID})
	if err != nil {
		return SessionActivity{}, err
	}
	return out.ByID[sessionID], nil
}

// SessionActivityBatch classifies every requested id in one batched pass.
// Every requested id has an entry; ids unknown to the session store still
// classify (from lock, drivers and live work alone).
func (app *App) SessionActivityBatch(ctx context.Context, ids []string) (SessionActivityBatchResult, error) {
	out := SessionActivityBatchResult{
		ByID:      make(map[string]SessionActivity, len(ids)),
		LockStems: make(map[string][]string, len(ids)),
	}
	if len(ids) == 0 {
		return out, nil
	}
	for _, id := range ids {
		stem := session.SessionLockStem(id)
		out.LockStems[stem] = append(out.LockStems[stem], id)
	}

	// Fact kind 1: driver markers, ALL rows, host liveness decided once per
	// distinct host. Unlike LiveSessionDrivers the dead hosts' rows are
	// kept: they are the crashed fact of decision 5 (the marker outlives
	// its host until the purge sweep).
	store := app.AsyncJobStore()
	liveDrivers, deadDrivers, driversErr := app.sessionDriverFacts(ctx, ids, store)
	driversFailed := driversErr != nil

	// Fact kind 2: reaction debt, one statement (same predicates as the
	// single-owner query, decision 13).
	debts, debtErr := map[string]bool{}, error(nil)
	if store != nil {
		debts, debtErr = store.ReactionDebtOwners(ctx, ids)
	}

	// Fact kind 3: live own jobs and descendant delegations, one batched
	// walk (one query per BFS level, one probe per distinct host).
	var work map[string]session.RootLiveWork
	if store != nil {
		work = store.LiveWorkForRoots(ctx, ids)
	}

	// Fact kind 4: RUNNING rows on provably DEAD hosts (decision 5), one
	// statement over the owners; live-host rows are already covered by the
	// walk above.
	deadRows, deadRowsErr := map[string]int{}, error(nil)
	if store != nil {
		deadRows, deadRowsErr = app.deadHostRunningJobs(ctx, store, ids)
	}

	// Fact kind 5: open once wake schedules, one statement.
	schedules, wakeErr := map[string][]session.OpenWakeSchedule{}, error(nil)
	if app.WakeScheduleStore() != nil {
		schedules, wakeErr = session.OpenOnceWakeSchedulesForOwners(ctx, app.WakeScheduleStore(), ids)
	}

	// Fact kind 6: ended reasons, one statement over the requested ids --
	// top-level AND child sessions, which the session list does not carry
	// (decision 7). A missing row is not an error and not an end fact.
	ends, endErr := app.sessionEndReasons(ctx, ids)

	for _, id := range ids {
		f := session.ActivityFacts{SessionID: id}
		if d, ok := liveDrivers[id]; ok {
			f.Driver = &d
		}
		f.DeadHostDriver = deadDrivers[id]
		switch {
		case driversFailed:
			f.Unreadable = append(f.Unreadable, "the session's driver markers")
		case debtErr != nil:
			f.Unreadable = append(f.Unreadable, "the session's reaction debt")
		default:
			f.DriverOwes = debts[id]
		}
		if store != nil {
			if rw, ok := work[id]; ok {
				f.OwnRunningJobs = len(rw.Own)
				f.LiveDelegations = len(rw.Descendants)
				if rw.OwnIncomplete {
					f.Unreadable = append(f.Unreadable, "the session's own jobs")
				}
				if rw.DescendantsIncomplete {
					f.Unreadable = append(f.Unreadable, "the session's delegations")
				}
			}
		} else {
			f.Unreadable = append(f.Unreadable, "the session's async jobs")
		}
		f.DeadHostRunningJobs = deadRows[id]
		if deadRowsErr != nil {
			f.Unreadable = append(f.Unreadable, "the session's running jobs")
		}
		if wakeErr != nil {
			f.Unreadable = append(f.Unreadable, "the session's wake schedules")
		} else {
			f.OpenSchedules = schedules[id]
		}
		switch {
		case endErr != nil:
			f.EndUnreadable = true
		default:
			f.EndedReason = ends[id]
		}
		f.Lock = session.InspectSessionLockFact(app.dataDir, id)

		v := session.ClassifySessionActivity(f)
		out.ByID[id] = SessionActivity{Facts: f, Verdict: v}
	}
	return out, nil
}

// sessionDriverFacts splits the marker rows into live drivers (keyed by
// session id) and dead-host markers (decision 5), deciding each distinct
// host's liveness once.
func (app *App) sessionDriverFacts(ctx context.Context, ids []string, store *session.AsyncJobStore) (live map[string]session.SessionDriver, dead map[string]bool, err error) {
	live = map[string]session.SessionDriver{}
	dead = map[string]bool{}
	conn := app.activityConn()
	if conn == nil {
		return live, dead, fmt.Errorf("session activity: no database handle")
	}
	rows, err := conn.ListSessionDrivers(ctx)
	if err != nil {
		return live, dead, fmt.Errorf("session activity: driver markers: %w", err)
	}
	hostStatus := make(map[string]session.HostLockStatus)
	statusOf := func(hostID string) session.HostLockStatus {
		if st, ok := hostStatus[hostID]; ok {
			return st
		}
		st := session.HostStatusUnknown
		if store != nil {
			st = store.HostLiveness(hostID)
		}
		hostStatus[hostID] = st
		return st
	}
	for _, row := range rows {
		if statusOf(row.HostID) == session.HostStatusDead {
			dead[row.SessionID] = true
			continue
		}
		live[row.SessionID] = session.SessionDriver{
			SessionID: row.SessionID, HostID: row.HostID,
			PID: int64(row.Pid), Status: statusOf(row.HostID),
		}
	}
	return live, dead, nil
}

// deadHostRunningJobs counts the session's own RUNNING rows whose host is
// provably dead (decision 5), one statement over the owners.
func (app *App) deadHostRunningJobs(ctx context.Context, store *session.AsyncJobStore, ids []string) (map[string]int, error) {
	rows, err := store.ListRunningAsyncJobsForOwners(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("session activity: running jobs: %w", err)
	}
	out := map[string]int{}
	seen := map[string]map[string]bool{}
	for _, row := range rows {
		if store.HostLiveness(row.HostID) != session.HostStatusDead {
			continue
		}
		if seen[row.OwnerSessionID] == nil {
			seen[row.OwnerSessionID] = map[string]bool{}
		}
		if seen[row.OwnerSessionID][row.HostID] {
			continue
		}
		seen[row.OwnerSessionID][row.HostID] = true
		out[row.OwnerSessionID]++
	}
	return out, nil
}

// sessionEndReasons reads ended_reason for every requested id in one
// statement. A deleted session has no row: no fact, no error (decision 7).
func (app *App) sessionEndReasons(ctx context.Context, ids []string) (map[string]string, error) {
	conn := app.activityConn()
	if conn == nil {
		return nil, fmt.Errorf("session activity: no database handle")
	}
	rows, err := conn.ListSessionEndReasonsForIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("session activity: end reasons: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.ID] = row.EndedReason
	}
	return out, nil
}

// activityConn is the read pool the batched fact queries run on, wrapped in
// the generated Queries (no hand-written SQL: decision 13).
func (app *App) activityConn() *db.Queries {
	conn := app.readDB
	if conn == nil && app.DB != nil {
		conn = app.DB()
	}
	if conn == nil {
		return nil
	}
	return db.New(conn)
}
