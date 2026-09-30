// The 60-second host-level pass (doc sec.3.4 rule (b), sec.3.5's "the ONLY
// inter-process fallback"): hints live only inside this process, so a host
// re-checks, once a minute: its own parked delegations (recheckChild, which
// may need to fire again if the child's own work was recovered by another
// process), every session whose launch is waiting (a Drain refused by an
// admission gate, paced by the launch gate, held by a rerun, or whose launch
// decision could not be read -- wakeSession's verdict switch keeps it in the
// set), and the sweep of idle per-session state. The wakes run detached (one
// per session, bounded concurrency) so a slow turn never delays the next tick.
// The web process starts the ticker (SetPersistentMode) and so does a `rush
// run` (ClaimExternalDriver): the CLI root itself is a hint-only no-op for the
// pass, but its delegated children's retries ride it. A `rush run` also runs
// RunMaintenanceSweep once at loop start and re-reads the DB itself
// (WaitForHint, at most every 5s) while its scope is open.
package agent

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// recheckPassIntervalNS is the ticker period in nanoseconds, atomic so a test
// can drive the real ticker at test timescale while a never-stopped ticker of
// an earlier test exists; production never changes it.
var recheckPassIntervalNS atomic.Int64

func init() { recheckPassIntervalNS.Store(int64(60 * time.Second)) }

// maxConcurrentRecheckWakes bounds how many detached recheck-pass wakes run
// at once. A wake that finds the bound full is put back in the recheck set
// and retried by the next pass.
const maxConcurrentRecheckWakes = 4

// recheckPassSweepSeam is a test-only hook called once per RecheckPass, right
// before the maintenance sweep. Empty in every production path; atomic
// because a never-stopped ticker of an earlier test may tick while a later
// test swaps it.
var recheckPassSweepSeam atomic.Pointer[func()]

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

// RecheckPass runs one iteration of the 60s host-level pass: the dead-host
// sweep and retention purge, a re-evaluation of every parked delegation
// (recheckChild -- a child whose scope was recovered or otherwise resolved by
// another process since it was parked), and a wake for every session in the
// recheck set (a hint that could not be acted on immediately). The wakes are
// DETACHED (launchRecheckWake): a Drain turn can take minutes, and the pass
// must return promptly so the next tick's sweep/purge/recheck run on
// schedule. Exported and independently callable so tests can drive it
// without waiting for the real ticker.
func (c *coordinator) RecheckPass(ctx context.Context) {
	if seam := recheckPassSweepSeam.Load(); seam != nil {
		(*seam)()
	}
	c.RunMaintenanceSweep(ctx)
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
	for _, sessionID := range c.drainRecheckSet() {
		if !c.launchRecheckWake(ctx, sessionID) {
			// Already being woken, or the concurrency bound is full: keep the
			// session for the next pass instead of forgetting the wake.
			c.addToRecheckSet(sessionID)
		}
	}
	// Last: a session the loop above just queued keeps its entry (its gate is
	// not zero, or it has jobs); only idle state is freed.
	c.sweepSessionState(ctx)
}

// launchRecheckWake starts sessionID's wake on its own goroutine and returns
// at once, or returns false without launching when a recheck wake for the
// session is already in flight or maxConcurrentRecheckWakes are running. The
// wake outlives the pass but not ctx (the ticker's context, cancelled by
// StopRecheckTicker/CancelAll). wakeSession re-adds sessionID to the recheck
// set itself while the launch is still not allowed (paced, deferred with a
// re-check, refused, an unreadable decision), so the launcher never duplicates
// that decision.
func (c *coordinator) launchRecheckWake(ctx context.Context, sessionID string) bool {
	c.recheckMu.Lock()
	if _, busy := c.recheckWakeInFlight[sessionID]; busy || len(c.recheckWakeInFlight) >= maxConcurrentRecheckWakes {
		c.recheckMu.Unlock()
		return false
	}
	if c.recheckWakeInFlight == nil {
		c.recheckWakeInFlight = make(map[string]struct{})
	}
	c.recheckWakeInFlight[sessionID] = struct{}{}
	c.recheckWakes.Add(1)
	c.recheckMu.Unlock()
	go func() {
		defer c.recheckWakes.Done()
		defer func() {
			c.recheckMu.Lock()
			delete(c.recheckWakeInFlight, sessionID)
			c.recheckMu.Unlock()
		}()
		if err := c.wakeSession(ctx, sessionID, false); err != nil {
			slog.Debug("coordinator: recheck pass wake attempt did not complete", "session_id", sessionID, "err", err)
		}
	}()
	return true
}

// waitRecheckWakes blocks until every detached recheck-pass wake launched so
// far has returned.
func (c *coordinator) waitRecheckWakes() {
	c.recheckWakes.Wait()
}

// StartRecheckTicker starts the 60s background pass exactly once per
// coordinator (idempotent), stopped by StopRecheckTicker (wired into
// CancelAll). Started by the web process (SetPersistentMode) and by a `rush
// run` loop's ClaimExternalDriver (see the file header).
func (c *coordinator) StartRecheckTicker() {
	c.recheckOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		c.recheckStop = cancel
		done := make(chan struct{})
		c.recheckDone = done
		go func() {
			defer close(done)
			ticker := time.NewTicker(time.Duration(recheckPassIntervalNS.Load()))
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
