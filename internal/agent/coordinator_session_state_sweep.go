// Freeing per-session in-memory state (R3B-8). A long-lived web process meets
// many sessions; the ledger's per-session entry (hint counter, launch gate,
// marker, job map, channel) and the coordinator's auto-turn maps used to live
// as long as the process. The 60s pass (RecheckPass) sweeps what nothing refers
// to any more. State that means something until a human message (Stop's
// suspension, the bg-shell cap counters) of a session that still exists is NOT
// freed: dropping it would re-arm automatic turns.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// sweepIdleSessions drops the ledger entries that hold nothing: no jobs and
// no external driver. (The launch gate moved to the arbiter state in R-ARB-2;
// its idle half is swept by sweepArbiterEntries below.) A dropped entry's
// hint counter starts again at zero, which is harmless: the gate is open, and
// a CLI waiter's session is an external driver (never swept).
func (l *workLedger) sweepIdleSessions() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for id, s := range l.bySession {
		if len(s.jobs) == 0 && !s.externalDriver {
			delete(l.bySession, id)
			n++
		}
	}
	return n
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
	if c.asyncJobs != nil {
		c.asyncJobs.sweepIdleSessions()
	}
	c.sweepArbiterEntries()
	c.sweepDeletedSessionState(ctx)
}
