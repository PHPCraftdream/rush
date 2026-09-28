// The 60-second host-level pass (doc sec.3.4 rule (b), sec.3.5's "the ONLY
// inter-process fallback"): hints live only inside this process, so a host
// re-checks, once a minute: its own parked delegations (recheckChild, which
// may need to fire again if the child's own work was recovered by another
// process), and every session a Drain submission was refused for (session-
// lock held elsewhere, shutdown) -- see coordinator_wake.go's
// turnAttemptRefused branch. This is the ONLY poll in the whole design; the
// CLI loop runs its own equivalent tick independently (internal/app's run
// loop), not this one.
package agent

import (
	"context"
	"log/slog"
	"time"
)

const recheckPassInterval = 60 * time.Second

// addToRecheckSet records sessionID so the next RecheckPass retries its
// Drain submission instead of losing the wake (doc sec.3.4 rule (b)).
func (c *coordinator) addToRecheckSet(sessionID string) {
	if sessionID == "" {
		return
	}
	c.recheckMu.Lock()
	if c.recheckSet == nil {
		c.recheckSet = make(map[string]struct{})
	}
	c.recheckSet[sessionID] = struct{}{}
	c.recheckMu.Unlock()
}

// drainRecheckSet atomically empties and returns the current recheck set.
func (c *coordinator) drainRecheckSet() []string {
	c.recheckMu.Lock()
	defer c.recheckMu.Unlock()
	if len(c.recheckSet) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.recheckSet))
	for id := range c.recheckSet {
		out = append(out, id)
	}
	c.recheckSet = nil
	return out
}

// RecheckPass runs one iteration of the 60s host-level pass: re-evaluates
// every parked delegation (recheckChild -- a child whose scope was recovered
// or otherwise resolved by another process since it was parked) and every
// session in the recheck set (a hint that could not be acted on
// immediately). Exported and independently callable so tests can drive it
// without waiting for the real ticker.
func (c *coordinator) RecheckPass(ctx context.Context) {
	if c.asyncJobs != nil {
		for _, parent := range c.parkedParentSessions() {
			c.asyncJobs.recheckChild(parent)
		}
	}
	for _, sessionID := range c.drainRecheckSet() {
		// wakeSession re-adds sessionID to the recheck set itself if this
		// attempt is refused again (its own turnAttemptRefused branch) --
		// this loop never needs to duplicate that decision.
		id := jobIdentity{owner: sessionID, toolCallID: "recheck-pass"}
		if err := c.wakeSession(ctx, id, true); err != nil {
			slog.Debug("coordinator: recheck pass wake attempt did not complete", "session_id", sessionID, "err", err)
		}
	}
}

// StartRecheckTicker starts the 60s background pass exactly once per
// coordinator (idempotent), stopped by StopRecheckTicker (wired into
// CancelAll). Intended for the long-lived web/interactive process --
// `rush run` never calls this (its loop has its own independent tick, doc
// sec.3.5).
func (c *coordinator) StartRecheckTicker() {
	c.recheckOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		c.recheckStop = cancel
		go func() {
			ticker := time.NewTicker(recheckPassInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c.RecheckPass(ctx)
				}
			}
		}()
	})
}

// StopRecheckTicker stops the 60s pass started by StartRecheckTicker, if
// any. Safe to call even if it was never started.
func (c *coordinator) StopRecheckTicker() {
	if c.recheckStop != nil {
		c.recheckStop()
	}
}
