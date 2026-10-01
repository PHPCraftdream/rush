package session

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The classifier's table oracle (plan R-ACT, rework): every row names the
// architect decision (D1-D10) or the review finding
// (docs/reviews/2026-09-30-async-phase4-round{5,6,7,8}.md) whose shape it
// pins, so a regression cannot silently re-open the class.
//
// Revert-check mutants (each breaks exactly the cited rows):
//   - swap the delegating/between_turns priority -> "D2 delegation plus own
//     job" and "D2 delegation plus driver" fail;
//   - make a dead recorded PID a Kind over live work (the pre-rework R6C-2
//     shape) -> "D1 dead PID plus live work" fails;
//   - let an end_turn finish end a quiet session -> "D4 end_turn without
//     ended_reason" fails;
//   - let debt without a driver keep a session live -> "D6 debt without
//     driver" fails;
//   - treat a fresh empty lock as held -> "D10 fresh empty lock is
//     released" fails;
//   - drop the dead-host crash fact -> "D5 dead-host marker" fails;
//   - drop the CrashedPID annotation -> "D1 dead PID plus live work"
//     (CrashedPID assert) fails.
func TestClassifySessionActivity(t *testing.T) {
	t.Parallel()
	liveDriver := &SessionDriver{SessionID: "s", HostID: "h", PID: 4242, Status: HostStatusAlive}

	tests := []struct {
		name       string
		facts      ActivityFacts
		kind       ActivityKind
		pid        int64
		crashedPID int64
		waits      []string
		end        string
	}{
		{
			// D2: delegating whenever a live delegation exists, whatever
			// else is live.
			name: "D2 delegation plus own job is delegating",
			facts: ActivityFacts{
				SessionID:       "s",
				OwnRunningJobs:  1,
				LiveDelegations: 1,
			},
			kind:  ActivityDelegating,
			waits: []string{WaitTasks},
		},
		{
			// D2: the driver does not demote a delegation either; its PID
			// still travels on the verdict.
			name: "D2 delegation plus driver is delegating with the driver PID",
			facts: ActivityFacts{
				SessionID:       "s",
				Driver:          liveDriver,
				LiveDelegations: 1,
			},
			kind: ActivityDelegating, pid: 4242,
		},
		{
			// D1: live work outranks a dead recorded PID -- the PID becomes
			// an annotation, the Kind stays live.
			name: "D1 dead PID plus live work is between turns with CrashedPID",
			facts: ActivityFacts{
				SessionID: "s",
				Lock:      LockFact{Kind: LockDead, PID: 999999},
				Driver:    liveDriver,
			},
			kind: ActivityBetweenTurns, pid: 4242, crashedPID: 999999,
		},
		{
			// D1/R6C-2 narrowed: a dead foreign PID is crashed only when
			// nothing is live.
			name: "R6C-2 dead PID, no live work, is crashed",
			facts: ActivityFacts{
				SessionID: "s",
				Lock:      LockFact{Kind: LockDead, PID: 999999},
			},
			kind: ActivityCrashed, crashedPID: 999999,
		},
		{
			// D3: release wipes the PID, so a recorded PID means the holder
			// never reached release -- the end_turn finish of a previous
			// turn must not demote it to ended.
			name: "D3 dead PID with end_turn finish stays crashed",
			facts: ActivityFacts{
				SessionID:   "s",
				Lock:        LockFact{Kind: LockDead, PID: 1234},
				EndedReason: "end_turn",
			},
			kind: ActivityCrashed, crashedPID: 1234,
		},
		{
			// D5: a marker on a provably dead host outlives the host until
			// the purge sweep -- a crash fact.
			name: "D5 dead-host driver marker is crashed",
			facts: ActivityFacts{
				SessionID:      "s",
				DeadHostDriver: true,
			},
			kind: ActivityCrashed,
		},
		{
			// D5: same for a running row on a dead host.
			name: "D5 running row on a dead host is crashed",
			facts: ActivityFacts{
				SessionID:           "s",
				DeadHostRunningJobs: 1,
			},
			kind: ActivityCrashed,
		},
		{
			// D4: ended_reason is the only end signal.
			name: "D4 ended_reason alone ends a quiet session",
			facts: ActivityFacts{
				SessionID:   "s",
				EndedReason: "canceled",
				Lock:        LockFact{Kind: LockReleased},
			},
			kind: ActivityEnded, end: "canceled",
		},
		{
			// D4: a clean finish is NOT an end -- kill -9 between turns
			// leaves ended_reason empty and the session must read idle, not
			// ended.
			name: "D4 end_turn without ended_reason is idle",
			facts: ActivityFacts{
				SessionID:   "s",
				Lock:        LockFact{Kind: LockReleased},
				EndedReason: "",
			},
			kind: ActivityIdle,
		},
		{
			// D6: debt keeps a session live only behind a live driver.
			name: "D6 debt without driver is not live",
			facts: ActivityFacts{
				SessionID:  "s",
				DriverOwes: true,
			},
			kind: ActivityIdle,
		},
		{
			// R5C-5: debt behind a live driver is a retry wait.
			name: "R5C-5 no lock, live driver, reaction debt pending",
			facts: ActivityFacts{
				SessionID: "s", Driver: liveDriver, DriverOwes: true,
			},
			kind: ActivityBetweenTurns, pid: 4242, waits: []string{WaitRetry},
		},
		{
			// R6C-1: a live loop waits on its job behind a released lock and
			// last turn's ended_reason -- not "ended".
			name: "R6C-1 released lock, ended_reason set, driver and own job live",
			facts: ActivityFacts{
				SessionID: "s", EndedReason: "end_turn",
				Lock:           LockFact{Kind: LockReleased},
				Driver:         liveDriver,
				OwnRunningJobs: 1,
			},
			kind: ActivityBetweenTurns, pid: 4242, waits: []string{WaitTasks},
		},
		{
			// D10: a freshly released (empty) lock is released, not in turn.
			name: "D10 fresh empty lock is released, not in turn",
			facts: ActivityFacts{
				SessionID: "s",
				Lock:      LockFact{Kind: LockReleased},
			},
			kind: ActivityIdle,
		},
		{
			// R7C-3: a live driver alone keeps the scope open.
			name: "R7C-3 released lock plus live driver is live for inject",
			facts: ActivityFacts{
				SessionID: "s",
				Lock:      LockFact{Kind: LockReleased},
				Driver:    liveDriver,
			},
			kind: ActivityBetweenTurns, pid: 4242,
		},
		{
			// R7C-2/R8C-5: the slug-id and reap shapes reduce to the same
			// facts -- a released lock plus a live marker stays live.
			name: "R7C-2/R8C-5 slug id, released lock, live marker",
			facts: ActivityFacts{
				SessionID: "fix/login-timeout",
				Lock:      LockFact{Kind: LockReleased},
				Driver:    liveDriver,
			},
			kind: ActivityBetweenTurns, pid: 4242,
		},
		{
			name: "live lock holder is in turn even with live work",
			facts: ActivityFacts{
				SessionID:      "s",
				Lock:           LockFact{Kind: LockHeld, PID: 777},
				Driver:         liveDriver,
				OwnRunningJobs: 1,
			},
			kind: ActivityInTurn, pid: 777,
		},
		{
			// ASYNC-02 fail-open: an unreadable lock is possibly live.
			name:  "lock read failure is fail-open in turn",
			facts: ActivityFacts{SessionID: "s", Lock: LockFact{Kind: LockUnknown}},
			kind:  ActivityInTurn, pid: 0,
		},
		{
			// The liveness convention: unreadable live-work facts count as
			// live, named in the verdict. (An unreadable END fact does NOT:
			// see the next row.)
			name: "unreadable live-work facts fail open between turns",
			facts: ActivityFacts{
				SessionID:  "s",
				Unreadable: []string{"the session's own jobs"},
			},
			kind:  ActivityBetweenTurns,
			waits: []string{WaitUnknown},
		},
		{
			// D7: an unreadable session row (end fact) confers no liveness.
			name: "D7 unreadable end fact does not keep the session live",
			facts: ActivityFacts{
				SessionID:     "s",
				EndUnreadable: true,
			},
			kind: ActivityIdle,
		},
		{
			name: "open once wake schedule keeps the session between turns",
			facts: ActivityFacts{
				SessionID: "s",
				Lock:      LockFact{Kind: LockReleased},
				OpenSchedules: []OpenWakeSchedule{
					{ID: "wake_1", NextRunAt: time.Unix(1700000000, 0).UTC()},
				},
			},
			kind:  ActivityBetweenTurns,
			waits: []string{WaitSchedule},
		},
		{
			// D8's reader half: a driver-read failure lands in Unreadable
			// and behaves like any unreadable live-work fact.
			name: "D8 unreadable drivers fail open between turns",
			facts: ActivityFacts{
				SessionID:  "s",
				Unreadable: []string{"the session's driver markers"},
			},
			kind:  ActivityBetweenTurns,
			waits: []string{WaitUnknown},
		},
		{
			name:  "no facts at all is idle",
			facts: ActivityFacts{SessionID: "fresh"},
			kind:  ActivityIdle,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			v := ClassifySessionActivity(tt.facts)
			require.Equal(t, tt.kind, v.Kind, "verdict: %s", v.Description)
			require.Equal(t, tt.pid, v.PID)
			require.Equal(t, tt.crashedPID, v.CrashedPID)
			require.Equal(t, tt.waits, v.WaitingOn)
			require.Equal(t, tt.end, v.EndedReason)
			require.Equal(t, tt.facts.Unreadable, v.Unreadable)
			require.NotEmpty(t, v.Description)
		})
	}
}

// The BetweenTurns description names the driver PID and the wait, and a
// dead-PID annotation rides along when live work outranks it (D1).
func TestClassifySessionActivity_Description(t *testing.T) {
	t.Parallel()
	next := time.Now().Add(4 * time.Minute).Truncate(time.Second)
	v := ClassifySessionActivity(ActivityFacts{
		SessionID:      "s",
		Driver:         &SessionDriver{SessionID: "s", PID: 4242},
		DriverOwes:     true,
		OwnRunningJobs: 2,
	})
	require.Equal(t, ActivityBetweenTurns, v.Kind)
	require.Contains(t, v.Description, "PID 4242")
	require.Contains(t, v.Description, "a reaction is owed")
	require.Contains(t, v.Description, "2 running job(s)")

	v = ClassifySessionActivity(ActivityFacts{
		SessionID:     "s",
		OpenSchedules: []OpenWakeSchedule{{ID: "wake_9", NextRunAt: next}},
	})
	require.Contains(t, v.Description, "wake schedule wake_9")

	v = ClassifySessionActivity(ActivityFacts{
		SessionID: "s",
		Lock:      LockFact{Kind: LockDead, PID: 999999},
		Driver:    &SessionDriver{SessionID: "s", PID: 4242},
	})
	require.Equal(t, ActivityBetweenTurns, v.Kind)
	require.Contains(t, v.Description, "dead PID 999999")
}

// The lock-fact enum reads the on-disk record, not the mtime (D10): a
// freshly released lock is released.
func TestInspectSessionLockFact(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()

	require.Equal(t, LockAbsent, InspectSessionLockFact(dataDir, "none").Kind)

	// A held lock: sidecar names this (alive) process.
	held := SessionLockPath(dataDir, "held")
	require.NoError(t, writeFile(held, "content\n"))
	require.NoError(t, writeFile(held+".pid", itoa2(os.Getpid())+"\n"))
	f := InspectSessionLockFact(dataDir, "held")
	require.Equal(t, LockHeld, f.Kind)
	require.Equal(t, os.Getpid(), f.PID)

	// A dead holder: a PID that is not alive. The record (primary file and
	// sidecar) survives a crash -- release is what wipes it.
	dead := SessionLockPath(dataDir, "dead")
	require.NoError(t, writeFile(dead, "999999\n"))
	require.NoError(t, writeFile(dead+".pid", "999999\n"))
	f = InspectSessionLockFact(dataDir, "dead")
	require.Equal(t, LockDead, f.Kind)
	require.Equal(t, 999999, f.PID)

	// A clean release: record truncated, sidecar removed -- even when the
	// mtime is fresh.
	released := SessionLockPath(dataDir, "released")
	require.NoError(t, writeFile(released, ""))
	f = InspectSessionLockFact(dataDir, "released")
	require.Equal(t, LockReleased, f.Kind)
}
