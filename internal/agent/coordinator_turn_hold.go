package agent

import "sync"

// AutoTurnHolder is the OPTIONAL Coordinator surface a rerun uses to keep
// automatic (Drain) turns off a session while it cancels the live turn,
// truncates history and hands the reservation to the replacement turn: a
// cancelled turn's release would otherwise start a paid Drain that races the
// rerun (it fails "still stopping", or its reaction is truncated and re-pended
// and paid twice). Type-asserted by the caller like ParkedSubAgentWorkReporter
// so the many Coordinator fakes stay untouched.
type AutoTurnHolder interface {
	// HoldAutomaticTurns refuses automatic turns for sessionID until the
	// returned func is called (idempotent). Holds nest: automatic turns resume
	// when the last hold is released. While held the launch predicate answers
	// drainPaced with no clock (a temporary hold, never "deferred": a delegated
	// child's parent keeps the delegation open), so a skipped wake is retried by
	// the re-check tick and the release retry after the hold ends.
	HoldAutomaticTurns(sessionID string) (release func())
}

// HoldAutomaticTurns implements AutoTurnHolder.
func (c *coordinator) HoldAutomaticTurns(sessionID string) (release func()) {
	c.autoResumeMu.Lock()
	if c.turnHolds == nil {
		c.turnHolds = make(map[string]int)
	}
	c.turnHolds[sessionID]++
	c.autoResumeMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.autoResumeMu.Lock()
			defer c.autoResumeMu.Unlock()
			if c.turnHolds[sessionID] <= 1 {
				delete(c.turnHolds, sessionID)
				return
			}
			c.turnHolds[sessionID]--
		})
	}
}

// automaticTurnsHeld reports whether a rerun currently holds sessionID's
// automatic turns.
func (c *coordinator) automaticTurnsHeld(sessionID string) bool {
	c.autoResumeMu.Lock()
	defer c.autoResumeMu.Unlock()
	return c.turnHolds[sessionID] > 0
}
