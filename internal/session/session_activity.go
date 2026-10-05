// One classifier for "is this session alive, and if so, on what" (plan
// R-ACT): every reader that today re-derives liveness from the lock file,
// the driver marker and the live-work rows -- sessions why/watch/tail/locks/
// reap/inject/list/cancel/reset/show and the web handlers -- feeds the same
// facts through ClassifySessionActivity and renders its verdict. The facts
// are collected by the app-layer reader (internal/app's session activity
// reader); this file is pure: no IO, no clock, no globals.
//
// Phase 4 holds the session lock only during a turn and truncates (never
// deletes) its file on release, so "no live lock" no longer means "ended".
// The durable facts that a scope is still open are the driver marker, the
// running job / delegation rows and the open once wake schedules. The
// codebase's liveness convention is fail-open: a fact that could not be
// read counts as live, never as ended. Unreadable END facts (the session
// row) never confer liveness -- only unreadable facts of live work do.
//
// Kind priority (architect decision): in_turn > delegating > between_turns
// > crashed > ended > idle. Live work outranks a dead foreign PID: the PID
// becomes an annotation (Verdict.CrashedPID), and crashed is only a Kind
// when nothing is live. A dead PID means the holder never reached release
// (release wipes the PID and the .pid sidecar), so it stays a crash even
// with an end_turn finish from a previous turn.

package session

import (
	"fmt"
	"sort"
	"strings"
)

// ActivityKind is the verdict of ClassifySessionActivity.
type ActivityKind string

const (
	// ActivityInTurn: the session lock is held by a live holder (a turn is
	// running, here or in another process), or its state could not be read
	// at all (fail-open).
	ActivityInTurn ActivityKind = "in_turn"
	// ActivityDelegating: at least one live sub-agent delegation is keeping
	// the session open, whatever else is live too.
	ActivityDelegating ActivityKind = "delegating"
	// ActivityBetweenTurns: no live lock and no live delegation, but other
	// durable work (a `rush run` loop, running jobs, schedules, a paced
	// retry behind a live driver) keeps the session open.
	ActivityBetweenTurns ActivityKind = "between_turns"
	// ActivityCrashed: nothing is live, but a crash fact exists -- a lock
	// recording a dead foreign PID (the holder never reached release), or a
	// driver marker / running row naming a provably dead host.
	ActivityCrashed ActivityKind = "crashed"
	// ActivityEnded: no live work, no crash, and the session row records an
	// ended_reason from the last `rush run`.
	ActivityEnded ActivityKind = "ended"
	// ActivityIdle: nothing is known to be happening and no end is recorded.
	ActivityIdle ActivityKind = "idle"
)

// The wait kinds of a BetweenTurns/Delegating verdict. A live delegation is
// not a wait item -- it IS the Delegating kind.
const (
	WaitTasks    = "tasks"    // the session's own running async jobs
	WaitSchedule = "schedule" // open once wake schedules
	WaitRetry    = "retry"    // reaction debt behind a LIVE driver
	WaitAnswer   = "answer"   // a held delegation's child paused on a question (#1158)
	WaitUnknown  = "unknown"  // a live-work fact that could not be read
)

// LockFactKind is the one-enum state of a session's lock file
// (architect decision: no independent CleanRelease/HolderAlive flag pair).
// Release truncates the lock file and removes the .pid sidecar
// (clearHolderMetadata), so a readable empty record IS a release whatever
// the mtime -- the old flag pair misread a fresh empty file as in turn for
// the 20s heartbeat window after every release.
type LockFactKind string

const (
	// LockAbsent: no lock file on disk.
	LockAbsent LockFactKind = "absent"
	// LockHeld: the record names a PID that is (probably) alive.
	LockHeld LockFactKind = "held"
	// LockReleased: a readable, empty record -- the leftover of a clean
	// release.
	LockReleased LockFactKind = "released"
	// LockDead: the record names a PID that is not alive (holder never
	// reached release).
	LockDead LockFactKind = "dead"
	// LockUnknown: the record could not be read (I/O error, Windows
	// mandatory lock with a missing sidecar). Fail-open: classified as in
	// turn.
	LockUnknown LockFactKind = "unknown"
)

// LockFact is the lock half of ActivityFacts.
type LockFact struct {
	Kind LockFactKind
	// PID is the recorded holder PID for held/dead, 0 otherwise.
	PID int
}

// ActivityFacts is the input of ClassifySessionActivity: everything a reader
// knows about one session at one moment. Zero fields mean "not present".
type ActivityFacts struct {
	SessionID string
	Lock      LockFact
	// Driver is the session's live `rush run` loop marker (a session_drivers
	// row whose host is alive or unknown), nil when none.
	Driver *SessionDriver
	// DriverOwes: durable reaction debt is open for this session. It keeps
	// the session live ONLY together with a live driver (decision 6): a
	// drain that failed and was then killed leaves debt behind, and that
	// debt must not resurrect a dead session.
	DriverOwes bool
	// OwnRunningJobs: the session's own running non-delegation async jobs.
	OwnRunningJobs int
	// LiveDelegations: live descendant delegation rows. Any positive count
	// makes the verdict Delegating (decision 2).
	LiveDelegations int
	// LiveDescendantSessionIDs lists the DISTINCT child sessions named by
	// those live delegation rows (the web list's LiveDescendantIDs wire
	// field), in walk order. Display data: the liveness decision is
	// LiveDelegations alone.
	LiveDescendantSessionIDs []string
	// OpenSchedules: the session's open once wake schedules.
	OpenSchedules []OpenWakeSchedule
	// ChildQuestions: the session's pending child_question notices (#1158)
	// -- held delegations whose child paused on a question. Purely additive
	// to the facts: readers that do not read notices leave it nil and the
	// verdict is exactly what it was before. Display data: it never makes a
	// live session out of a dead one (the held delegation row already
	// does), it only names WHAT the wait is.
	ChildQuestions []ChildQuestion
	// DeadHostDriver: a session_drivers row names a provably dead host (a
	// crash fact: the loop died before its marker was purged).
	DeadHostDriver bool
	// DeadHostRunningJobs: the session owns running async_jobs rows on a
	// provably dead host.
	DeadHostRunningJobs int
	// EndedReason is the session row's ended_reason: the last `rush run`'s
	// exit reason, or "" while running / never run. The ONLY end signal
	// (decision 4): a clean end_turn finish without it is idle, because a
	// kill -9 between turns leaves ended_reason empty.
	EndedReason string
	// EndUnreadable: the session row could not be read. Recorded for the
	// description but deliberately NOT liveness: an unreadable end fact
	// never keeps a session open (decision 7).
	EndUnreadable bool
	// Unreadable lists the LIVE-WORK facts a reader could not read at all.
	// Unknown counts as live, so any entry keeps the session open.
	Unreadable []string
}

// ActivityVerdict is the classification of one session.
type ActivityVerdict struct {
	Kind ActivityKind
	// PID is the lock holder's PID for in_turn and the driver loop's for
	// between_turns/delegating (0 when unknown).
	PID int64
	// CrashedPID is the dead recorded PID when the session is live (or
	// crashed) despite a lock recording a dead holder. An annotation, never
	// the Kind (decision 1).
	CrashedPID int64
	// WaitingOn lists what keeps a BetweenTurns/Delegating session open
	// besides its delegations, sorted.
	WaitingOn []string
	// EndedReason is the reason of an Ended verdict.
	EndedReason string
	// Unreadable echoes the live-work facts that could not be read.
	Unreadable []string
	// Description is the human-readable one-line rendering.
	Description string
}

// ClassifySessionActivity reduces one session's facts to a verdict. Rule
// order is the Kind priority; each rule names the architect decision or the
// review finding that pinned it:
//
//  1. An unreadable lock is in turn -- fail-open (ASYNC-02).
//  2. Live work: delegating when any live delegation exists (decision 2),
//     between_turns otherwise. A dead recorded PID is an annotation here
//     (decision 1, narrowing R6C-2), not a Kind.
//  3. Crash: a dead foreign PID with no live work (R6C-2 narrowed), or a
//     driver marker / running row on a provably dead host (decision 5).
//  4. Ended only by ended_reason (decision 4); a finish reason is not an
//     end signal, so the reader no longer reads messages at all.
//  5. Idle.
func ClassifySessionActivity(f ActivityFacts) ActivityVerdict {
	v := ActivityVerdict{Unreadable: f.Unreadable}

	// 1. In turn; an unreadable lock is fail-open (ASYNC-02).
	if f.Lock.Kind == LockUnknown || f.Lock.Kind == LockHeld {
		pid := int64(f.Lock.PID)
		if pid == 0 && f.Driver != nil {
			pid = f.Driver.PID
		}
		v.Kind = ActivityInTurn
		v.PID = pid
		if f.Lock.Kind == LockUnknown {
			v.Description = "in turn (lock state could not be read; assuming live)"
		} else {
			v.Description = fmt.Sprintf("in turn (held by PID %d)", pid)
		}
		return v
	}

	// 2. Live work between turns.
	waits := waitingOn(f)
	if live := liveWork(f); live {
		pid := int64(0)
		if f.Driver != nil {
			pid = f.Driver.PID
		}
		v.PID = pid
		v.WaitingOn = waits
		if f.Lock.Kind == LockDead {
			v.CrashedPID = int64(f.Lock.PID)
		}
		if f.LiveDelegations > 0 {
			v.Kind = ActivityDelegating
			v.Description = describeDelegating(f, v.CrashedPID)
			return v
		}
		v.Kind = ActivityBetweenTurns
		v.Description = describeBetweenTurns(f, waits, v.CrashedPID)
		return v
	}

	// 3. Crash: a recorded dead PID (holder never reached release) or a
	// dead-host marker/row, with nothing live.
	if f.Lock.Kind == LockDead {
		v.Kind = ActivityCrashed
		v.CrashedPID = int64(f.Lock.PID)
		v.Description = fmt.Sprintf("crashed: lock held by PID %d, which is no longer alive", f.Lock.PID)
		return v
	}
	if f.DeadHostDriver || f.DeadHostRunningJobs > 0 {
		v.Kind = ActivityCrashed
		v.Description = "crashed: driver marker or running job names a dead host"
		return v
	}

	// 4. Ended only by a recorded ended_reason (decision 4).
	if f.EndedReason != "" {
		v.Kind = ActivityEnded
		v.EndedReason = f.EndedReason
		v.Description = fmt.Sprintf("ended (%s)", f.EndedReason)
		return v
	}

	// 5. Idle. A missing session row is not an end fact and not unreadable
	// liveness: a deleted session is simply idle here (decision 7).
	if f.EndUnreadable {
		v.Kind = ActivityIdle
		v.Description = "idle: no live work (the session row could not be read)"
		return v
	}
	v.Kind = ActivityIdle
	v.Description = "idle: no live work and no recorded end"
	return v
}

// liveWork reports whether any durable fact keeps the session open.
func liveWork(f ActivityFacts) bool {
	return f.Driver != nil || f.OwnRunningJobs > 0 || f.LiveDelegations > 0 ||
		len(f.OpenSchedules) > 0 || (f.Driver != nil && f.DriverOwes) ||
		len(f.Unreadable) > 0
}

// waitingOn names every durable wait besides a live delegation (which is
// its own Kind), plus one "unknown" entry per unreadable live-work fact.
func waitingOn(f ActivityFacts) []string {
	var waits []string
	if f.OwnRunningJobs > 0 {
		waits = append(waits, WaitTasks)
	}
	if len(f.OpenSchedules) > 0 {
		waits = append(waits, WaitSchedule)
	}
	if f.Driver != nil && f.DriverOwes {
		waits = append(waits, WaitRetry)
	}
	if len(f.ChildQuestions) > 0 {
		waits = append(waits, WaitAnswer)
	}
	for range f.Unreadable {
		waits = append(waits, WaitUnknown)
	}
	sort.Strings(waits)
	return waits
}

func describeDelegating(f ActivityFacts, crashedPID int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "waiting on %d live sub-agent delegation(s)", f.LiveDelegations)
	if f.Driver != nil {
		fmt.Fprintf(&b, " (driven by `rush run` PID %d)", f.Driver.PID)
	}
	if clause := describeChildQuestions(f); clause != "" {
		fmt.Fprintf(&b, "; %s", clause)
	}
	if crashedPID > 0 {
		fmt.Fprintf(&b, "; a lock records dead PID %d", crashedPID)
	}
	return b.String()
}

// describeChildQuestions renders the awaiting-answer clause of a verdict
// whose held delegation(s) have a child paused on a question (#1158).
func describeChildQuestions(f ActivityFacts) string {
	if len(f.ChildQuestions) == 0 {
		return ""
	}
	q := f.ChildQuestions[0]
	return fmt.Sprintf("waiting for your answer: child %s asked: %s", q.ChildSessionID, q.Question)
}

func describeBetweenTurns(f ActivityFacts, waits []string, crashedPID int64) string {
	var parts []string
	if f.Driver != nil {
		parts = append(parts, fmt.Sprintf("driven by `rush run` (PID %d)", f.Driver.PID))
	}
	for _, w := range waits {
		switch w {
		case WaitTasks:
			parts = append(parts, fmt.Sprintf("waiting on %d running job(s)", f.OwnRunningJobs))
		case WaitSchedule:
			items := make([]string, 0, len(f.OpenSchedules))
			for _, s := range f.OpenSchedules {
				items = append(items, fmt.Sprintf("wake schedule %s at %s",
					s.ID, s.NextRunAt.Local().Format("15:04:05")))
			}
			parts = append(parts, "waiting on "+strings.Join(items, ", "))
		case WaitRetry:
			parts = append(parts, "a reaction is owed")
		case WaitUnknown:
			for _, u := range f.Unreadable {
				parts = append(parts, "could not read "+u+" (assuming it is live)")
			}
		}
	}
	if clause := describeChildQuestions(f); clause != "" {
		parts = append(parts, clause)
	}
	if crashedPID > 0 {
		parts = append(parts, fmt.Sprintf("a lock records dead PID %d", crashedPID))
	}
	if len(parts) == 0 {
		return "between turns"
	}
	return "between turns: " + strings.Join(parts, "; ")
}
