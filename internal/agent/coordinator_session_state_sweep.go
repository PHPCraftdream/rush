// Freeing per-session in-memory state (R3B-8). A long-lived web process meets
// many sessions; the rehomed per-session runtime state (the hint counter and
// external-driver mirror, batch_rehome_coord_runtime.go) and the
// coordinator's auto-turn maps used to live as long as the process. The 60s
// pass (RecheckPass) sweeps what nothing refers to any more. State that means something until a human message (Stop's
// suspension, the bg-shell cap counters) of a session that still exists is NOT
// freed: dropping it would re-arm automatic turns.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// sweepIdleSessions drops the rehomed per-session runtime state that holds
// nothing: no external-driver mirror entry (batch_rehome_coord_runtime.go).
// (The launch gate moved to the arbiter state in R-ARB-2; its idle half is
// swept by sweepArbiterEntries below.) A dropped entry's hint counter starts
// again at zero -- harmless only while the gate is open: while a dormant gate
// records HintSeen, dropping the counter makes gateShut read the reset
// counter as "a newer fact" and reopens the gate with no fact at all. Entries
// whose gate still holds hint state are kept until it does not. The check is
// best effort: a pace that records its hint between the gate check and the
// delete can still lose the counter once (the cost is one extra gate
// evaluation; the next pace re-records the hint).
func (c *coordinator) sweepIdleSessions() int {
	candidates := c.driverHints.idleCandidates()
	dropped := 0
	for _, id := range candidates {
		if c.arb.gateHoldsHint(id) {
			continue
		}
		// Re-check under the lock: a driver may have arrived while the
		// arbiter was asked.
		if c.driverHints.dropIfIdle(id) {
			dropped++
		}
	}
	return dropped
}

// sweepArbiterEntries frees arbiter entries whose every field is idle (no
// hold, no suspension, no cap counter, no chain, an expired-or-zero gate):
// such an entry is an open gate, exactly like a zero one. State that means
// something until a human message (a suspension, a spent cap) is kept.
func (c *coordinator) sweepArbiterEntries() {
	c.sweepArbiterEntriesAt(time.Now())
}

func (c *coordinator) sweepArbiterEntriesAt(now time.Time) {
	for _, id := range c.arb.sessionsWithState() {
		c.arb.dropIfIdle(id, now)
	}
}

// sweepDeletedSessionState drops the arbiter state of sessions that no longer
// exist (deleted): the only moment that state stops meaning anything without
// a human message.
func (c *coordinator) sweepDeletedSessionState(ctx context.Context) {
	if c.sessions == nil {
		return
	}
	ids := c.arb.sessionsWithState()
	if len(ids) == 0 {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, id := range ids {
		if _, err := c.sessions.Get(readCtx, id); !errors.Is(err, sql.ErrNoRows) {
			continue // alive, or unreadable right now: keep it
		}
		c.arb.dropSession(id)
	}
}

// sweepSessionState is the RecheckPass hook: all halves, best effort.
func (c *coordinator) sweepSessionState(ctx context.Context) {
	c.sweepIdleSessions()
	c.sweepArbiterEntries()
	c.sweepDeletedSessionState(ctx)
}
