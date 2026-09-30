// Freeing per-session in-memory state (R3B-8). A long-lived web process meets
// many sessions; the ledger's per-session entry (hint counter, launch gate,
// marker, job map, channel) and the coordinator's auto-turn maps used to live
// as long as the process. The 60s pass (RecheckPass) sweeps what nothing refers
// to any more. State that means something until a human message (Stop's
// suspension, the bg-shell cap counter) of a session that still exists is NOT
// freed: dropping it would re-arm automatic turns.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// sweepIdleSessions drops the ledger entries that hold nothing: no jobs, a
// zero launch gate, no external driver. A dropped entry's hint counter starts
// again at zero, which is harmless: the gate is zero, and a CLI waiter's
// session is an external driver (never swept).
func (l *workLedger) sweepIdleSessions() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for id, s := range l.bySession {
		g := s.drain
		if len(s.jobs) == 0 && !s.externalDriver && g.retryAt.IsZero() && g.freeStreak == 0 && g.paidStreak == 0 {
			delete(l.bySession, id)
			n++
		}
	}
	return n
}

// sweepDeletedSessionState drops the bg-shell cap counter and Stop's
// suspension of sessions that no longer exist (deleted): the only moment that
// state stops meaning anything without a human message.
func (c *coordinator) sweepDeletedSessionState(ctx context.Context) {
	if c.sessions == nil {
		return
	}
	c.autoResumeMu.Lock()
	ids := make(map[string]struct{}, len(c.consecutiveAutoResumes)+len(c.autoTurnsSuspended))
	for id := range c.consecutiveAutoResumes {
		ids[id] = struct{}{}
	}
	for id := range c.autoTurnsSuspended {
		ids[id] = struct{}{}
	}
	c.autoResumeMu.Unlock()
	if len(ids) == 0 {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for id := range ids {
		if _, err := c.sessions.Get(readCtx, id); !errors.Is(err, sql.ErrNoRows) {
			continue // alive, or unreadable right now: keep it
		}
		c.autoResumeMu.Lock()
		delete(c.consecutiveAutoResumes, id)
		delete(c.autoTurnsSuspended, id)
		c.autoResumeMu.Unlock()
	}
}

// sweepSessionState is the RecheckPass hook: both halves, best effort.
func (c *coordinator) sweepSessionState(ctx context.Context) {
	if c.asyncJobs != nil {
		c.asyncJobs.sweepIdleSessions()
	}
	c.sweepDeletedSessionState(ctx)
}
