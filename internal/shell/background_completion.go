package shell

// The completion hold: a job whose completion callback (OnDone) is registered
// but has neither recorded its outcome nor returned. A finished job leaves
// ActiveOwned the moment its process exits, yet the higher layer only learns
// the outcome durably later (the agent's callback writes a notice row, possibly
// after waiting on shared locks). PendingCompletionsOwned reports that gap so
// "this session still has work" stays true across it. The hold is per shell,
// taken at OnDone registration (before the process can finish, or while the
// registering call still runs), and ends at the first of: the callback calling
// MarkCompletionRecorded, the last registered callback returning (a panic
// included), or completionHoldMax after the shell finished.

import "time"

// completionHoldMax bounds how long a FINISHED shell's hold counts, whatever
// its callback is doing: a callback wedged on a dead dependency must not keep
// its session's scope open forever.
const completionHoldMax = 10 * time.Minute

// takeCompletionHold adds the shell to its manager's hold set. Idempotent.
func (bs *BackgroundShell) takeCompletionHold() {
	m := bs.mgr
	if m == nil {
		return
	}
	m.holdMu.Lock()
	if m.holds == nil {
		m.holds = make(map[*BackgroundShell]struct{})
	}
	m.holds[bs] = struct{}{}
	m.holdMu.Unlock()
}

// releaseCompletionHold drops the shell from its manager's hold set. Idempotent.
func (bs *BackgroundShell) releaseCompletionHold() {
	m := bs.mgr
	if m == nil {
		return
	}
	m.holdMu.Lock()
	delete(m.holds, bs)
	m.holdMu.Unlock()
}

// MarkCompletionRecorded ends the shell's completion hold: the caller (a
// completion callback) has made the outcome durable, so the session's own
// state now says what is owed and the job no longer needs to count as pending.
// Call it BEFORE any re-check the callback triggers. Safe from any goroutine,
// idempotent, and a no-op for a shell without a registered callback.
func (bs *BackgroundShell) MarkCompletionRecorded() {
	bs.releaseCompletionHold()
}

// PendingCompletionsOwned counts sessionID's jobs whose completion is still in
// flight: a callback registered and neither recorded nor returned. It is
// independent of the manager's job table (a killed or removed job still counts
// until its callback is done) and complements ActiveOwned, which stops counting
// a job as soon as its process exits. A finished job counts for at most
// completionHoldMax.
func (m *BackgroundShellManager) PendingCompletionsOwned(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	now := time.Now().Unix()
	m.holdMu.Lock()
	defer m.holdMu.Unlock()
	pending := 0
	for bs := range m.holds {
		if bs.SessionID != sessionID {
			continue
		}
		if at := bs.completedAt.Load(); at > 0 && now-at > int64(completionHoldMax/time.Second) {
			delete(m.holds, bs)
			continue
		}
		pending++
	}
	return pending
}
