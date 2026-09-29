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
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
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
	if c.asyncJobs != nil && c.asyncJobs.store != nil {
		// Doc sec.3.6/3.7: the host-level sweep over every dead host with a
		// running row -- the only cross-process fallback for a host that
		// never gets a turn/scope-evaluation of its own to trigger own-scope
		// recovery. Runs before recheckChild below so a delegation whose
		// child's own dead-host rows this same pass just recovered is
		// re-evaluated against already-fresh state.
		if _, err := c.asyncJobs.store.SweepDeadHosts(ctx, c.messages); err != nil {
			slog.Debug("coordinator: recheck pass dead-host sweep failed", "err", err)
		}
		// Retention (doc sec.3.7): old delivered/voided rows and empty dead
		// hosts' lock files, same pass, best-effort.
		if err := c.asyncJobs.store.PurgeExpired(ctx, session.AsyncDataRetentionAge); err != nil {
			slog.Debug("coordinator: recheck pass retention purge failed", "err", err)
		}
	}
	if c.asyncJobs != nil {
		// B12/C14 fix: recheckChild indexes l.byChild by CHILD session id
		// (never by parent/owner) -- parkedParentSessions() returns the
		// PARENT ids, so calling recheckChild with those was always a
		// byChild[parent] miss: no parked delegation was ever actually
		// re-evaluated by this pass. parkedChildSessions() returns the ids
		// recheckChild actually expects.
		for _, child := range c.asyncJobs.parkedChildSessions() {
			c.asyncJobs.recheckChild(child)
		}
	}
	// B12/C14 fix: each wake runs in its OWN goroutine so a real Drain turn
	// (which can take seconds) never serializes behind the others, or delays
	// the NEXT tick's dead-host sweep/retention purge above (this whole
	// RecheckPass call is one synchronous unit from StartRecheckTicker's own
	// loop). wg bounds this call's own return to "every wake was at least
	// SUBMITTED", matching every other fire-and-forget wakeSession call site
	// in this package (coordinator_background.go, supervision.go) -- none of
	// them wait for the turn to finish either.
	var wg sync.WaitGroup
	for _, sessionID := range c.drainRecheckSet() {
		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()
			// wakeSession re-adds sessionID to the recheck set itself if
			// this attempt is refused again (its own turnAttemptRefused
			// branch) -- this loop never needs to duplicate that decision.
			id := jobIdentity{owner: sessionID, toolCallID: "recheck-pass"}
			if err := c.wakeSession(ctx, id, true); err != nil {
				slog.Debug("coordinator: recheck pass wake attempt did not complete", "session_id", sessionID, "err", err)
			}
		}(sessionID)
	}
	wg.Wait()
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
		done := make(chan struct{})
		c.recheckDone = done
		go func() {
			defer close(done)
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
