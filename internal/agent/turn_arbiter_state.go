// The arbiter's mutable state (docs/plans/2026-10-01-turn-arbiter.md
// sec.2.3, R-ARB-2): ONE struct per session under ONE mutex, aggregating what
// used to be the coordinator maps (auto-resume counter, over-cap set,
// suspension set, rerun holds, the reaction-chain trio) plus the workLedger
// launch gate. Writers: accountDrainAttempt, noteDrainRefused, the
// human-message reset, HoldAutomaticTurns, suspendAutoResume, the reaction
// chain accounting and the bg-shell arrival. Readers go through
// arbiter.snapshot -- one capture, copied to a value (never two owner locks
// at once).
package agent

import (
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// arbiterState is one session's launch-decision state. gate.RetryAt zero
// means the gate is open (Paced is derived on the snapshot).
type arbiterState struct {
	holds           int // nested rerun holds (HoldAutomaticTurns)
	suspended       bool
	autoResumes     int
	overCap         map[int64]struct{}
	chainLinks      int
	chainClaims     map[string]struct{}
	chainNoticed    bool
	chainLastLaunch string
	// sleepAll is await_tasks' "until: all" sleep (#1270): armed by the
	// tool, cleared by every allowed drain launch, a human message and
	// Stop. sleepWakeID remembers the sleep's pending max_wait once
	// schedule so the clear paths can cancel it (an uncancelled schedule
	// keeps OnceWakeOpen set and holds the run open).
	sleepAll    bool
	sleepWakeID string
	gate        GateFacts
}

// arbiter owns every arbiterState. The zero value is ready to use.
type arbiter struct {
	mu        sync.Mutex
	bySession map[string]*arbiterState
}

func (a *arbiter) state(sid string) *arbiterState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stateLocked(sid)
}

func (a *arbiter) stateLocked(sid string) *arbiterState {
	s := a.bySession[sid]
	if s == nil {
		s = &arbiterState{}
		if a.bySession == nil {
			a.bySession = make(map[string]*arbiterState)
		}
		a.bySession[sid] = s
	}
	return s
}

// arbiterSnapshot is the value readTurnFacts' in-process half is built from.
// The sets are copied: the caller compares them against DB reads outside the
// lock.
type arbiterSnapshot struct {
	Held         bool
	Suspended    bool
	AutoResumes  int
	OverCap      map[int64]struct{}
	ChainLinks   int
	ChainClaims  map[string]struct{}
	ChainNoticed bool
	SleepAll     bool
	Gate         GateFacts
}

// snapshot copies sid's state under ONE lock capture.
func (a *arbiter) snapshot(sid string) arbiterSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return arbiterSnapshot{}
	}
	out := arbiterSnapshot{
		Held:         s.holds > 0,
		Suspended:    s.suspended,
		AutoResumes:  s.autoResumes,
		ChainLinks:   s.chainLinks,
		ChainNoticed: s.chainNoticed,
		SleepAll:     s.sleepAll,
		Gate:         s.gate,
	}
	out.Gate.Paced = !s.gate.RetryAt.IsZero()
	if len(s.overCap) > 0 {
		out.OverCap = make(map[int64]struct{}, len(s.overCap))
		for id := range s.overCap {
			out.OverCap[id] = struct{}{}
		}
	}
	if len(s.chainClaims) > 0 {
		out.ChainClaims = make(map[string]struct{}, len(s.chainClaims))
		for claim := range s.chainClaims {
			out.ChainClaims[claim] = struct{}{}
		}
	}
	return out
}

// hold takes a nested rerun hold; the returned func releases it (idempotent
// use of the returned func is the caller's business, as before).
func (a *arbiter) hold(sid string) (release func()) {
	a.mu.Lock()
	a.stateLocked(sid).holds++
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			if s := a.bySession[sid]; s != nil && s.holds > 0 {
				s.holds--
				if s.holds == 0 && s.idle(time.Now()) {
					delete(a.bySession, sid)
				}
			}
			a.mu.Unlock()
		})
	}
}

func (a *arbiter) held(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	return s != nil && s.holds > 0
}

func (a *arbiter) suspend(sid string) {
	a.mu.Lock()
	a.stateLocked(sid).suspended = true
	a.mu.Unlock()
}

func (a *arbiter) suspended(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	return s != nil && s.suspended
}

// setSleepAll arms await_tasks' "until: all" sleep for sid, remembering
// the max_wait once schedule id (empty when none) so the clear paths can
// cancel it.
func (a *arbiter) setSleepAll(sid, wakeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	s.sleepAll = true
	if wakeID != "" {
		s.sleepWakeID = wakeID
	}
}

// clearSleepAll disarms the sleep and returns the pending max_wait schedule
// id ("" when there is none) for the caller to cancel.
func (a *arbiter) clearSleepAll(sid string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return ""
	}
	id := s.sleepWakeID
	s.sleepAll = false
	s.sleepWakeID = ""
	return id
}

// autoResumesOf returns the bg-shell auto-resume counter since the last human
// message.
func (a *arbiter) autoResumesOf(sid string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return 0
	}
	return s.autoResumes
}

// claimAutoResumeSlot spends one bg-shell auto-resume slot at a completion's
// admission: eligible is the caller's policy answer (persistent web
// coordinator with AutoResumeOnJobDone on); a completion refused for a spent
// cap is recorded by its notice row id (rowID 0: the insert failed, nothing
// exists to defer). A Stop-suspended session spends nothing. This is THE one
// writer of the cap counter (R2B-16).
func (a *arbiter) claimAutoResumeSlot(sid string, rowID int64, eligible bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	if s.autoResumes >= turnAutoResumeCap {
		if rowID != 0 {
			if s.overCap == nil {
				s.overCap = make(map[int64]struct{})
			}
			s.overCap[rowID] = struct{}{}
		}
		return false
	}
	if !eligible || s.suspended {
		return false
	}
	s.autoResumes++
	return true
}

// pruneOverCapLocked drops over-cap ids that are no longer debt. notices must
// be the COMPLETE notice debt. Caller holds a.mu.
func (a *arbiter) pruneOverCapLocked(sid string, notices []session.PendingNoticeDebt) {
	s := a.bySession[sid]
	if s == nil || len(s.overCap) == 0 {
		return
	}
	owed := make(map[int64]struct{}, len(notices))
	for _, n := range notices {
		owed[n.ID] = struct{}{}
	}
	for id := range s.overCap {
		if _, ok := owed[id]; !ok {
			delete(s.overCap, id)
		}
	}
	if s.idle(time.Now()) {
		delete(a.bySession, sid)
	}
}

// pruneOverCap is the best-effort prune at a completion's arrival.
func (a *arbiter) pruneOverCap(sid string, notices []session.PendingNoticeDebt) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneOverCapLocked(sid, notices)
}

// pace shuts sid's gate for wait (the old gate pacing). It reports the
// moment a streak reached dormancy; kind says which streak the outcome counts
// toward (a paid attempt proves the pull works again, so it clears the free
// streak).
func (a *arbiter) pace(sid string, hintAt uint64, wait time.Duration, hintOpens bool, kind drainPace) (becameDormant bool) {
	if sid == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g := &a.stateLocked(sid).gate
	g.RetryAt = time.Now().Add(wait)
	g.HintSeen = hintAt
	g.HintOpens = hintOpens
	switch kind {
	case paceFreeNoTurn:
		g.FreeStreak++
		return g.FreeStreak == turnDormantStreak
	case pacePaidUnreacted:
		g.FreeStreak = 0
		g.PaidStreak++
		return g.PaidStreak == turnDormantStreak
	}
	return false
}

// resetGate opens sid's gate and clears its streaks.
func (a *arbiter) resetGate(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.bySession[sid]; s != nil {
		s.gate = GateFacts{}
	}
}

// resetForHumanMessage clears EVERYTHING one human message re-arms: the
// bg-shell cap and its over-cap set, Stop's suspension, the reaction chain,
// await_tasks' sleep flag with its remembered schedule id, and the launch
// gate. A rerun's hold is NOT a human message's to lift: the
// holds survive the reset and the entry dies only when the last hold is
// released (P1-2 of the R-ARB-2 review).
func (a *arbiter) resetForHumanMessage(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return
	}
	holds := s.holds
	*s = arbiterState{holds: holds}
	if holds == 0 {
		delete(a.bySession, sid)
	}
}

// chainLink accounts one finished Drain leg (the old per-leg link accounting):
// progress resets; self-sustaining idle completions increment; idle launches
// answering another fact start a fresh chain at 1; anything else leaves the
// count as it is. A successful reaction deliberately does NOT reset the count
// -- closing the chain's own completions is the event that masks the chain.
func (a *arbiter) chainLink(sid string, att *drainAttempt, snap session.DebtSnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	if att.chainProgress {
		last := s.chainLastLaunch
		a.resetChainLocked(sid, s)
		s.chainLastLaunch = last
		return
	}
	if len(att.chainIdleClaims) == 0 {
		return // a text-only or neutral-tool leg breaks nothing and adds nothing
	}
	claims := s.chainClaims
	if claims == nil {
		claims = make(map[string]struct{}, len(att.chainIdleClaims))
		s.chainClaims = claims
	}
	selfSustaining := true
	for _, ref := range snap.Jobs {
		if _, ok := claims[ref.ClaimID]; !ok {
			selfSustaining = false
			break
		}
	}
	if len(snap.Notices) > 0 {
		selfSustaining = false // another fact (bg-shell done, supervision, ...)
	}
	if selfSustaining {
		s.chainLinks++
	} else {
		s.chainLinks = 1
	}
	// A recorded launch must survive this launch-only leg so the next leg can
	// compare its command. A launch mixed with real action is not a chain link.
	for _, claim := range att.chainIdleClaims {
		claims[claim] = struct{}{}
	}
}

func (a *arbiter) resetChainLocked(sid string, s *arbiterState) {
	s.chainLinks = 0
	s.chainClaims = nil
	s.chainNoticed = false
	s.chainLastLaunch = ""
}

// chainLastLaunch reports whether command matches the immediately preceding
// async command launch in the session and records this launch under arbiter.mu.
func (a *arbiter) chainLastLaunch(sid, command string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	repeated := command != "" && s.chainLastLaunch == command
	s.chainLastLaunch = command
	return repeated
}

// resetChain clears the chain state (a human message, real progress).
func (a *arbiter) resetChain(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.bySession[sid]; s != nil {
		a.resetChainLocked(sid, s)
	}
}

// chainLinksOf returns the current link count.
func (a *arbiter) chainLinksOf(sid string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return 0
	}
	return s.chainLinks
}

// chainHasClaim reports whether claim belongs to the session's current
// idle-launch chain.
func (a *arbiter) chainHasClaim(sid, claim string) bool {
	if claim == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return false
	}
	_, ok := s.chainClaims[claim]
	return ok
}

// markChainNoticed records that the guard warned about sid; it reports
// whether it was ALREADY noticed (the caller inserts the marker only on the
// first hit).
func (a *arbiter) markChainNoticed(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	if s.chainNoticed {
		return true
	}
	s.chainNoticed = true
	return false
}

// sessionsWithState lists the sessions the arbiter holds any state for (the
// sweep's candidates).
func (a *arbiter) sessionsWithState() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.bySession))
	for id := range a.bySession {
		out = append(out, id)
	}
	return out
}

// dropIfIdle deletes sid's entry when nothing in it means anything any more
// (no hold, no suspension, no cap counter, no chain, no await_tasks sleep,
// and a gate that is open:
// never paced, or its pause has passed with both streaks at zero -- R4B-4).
// A suspended session or a spent cap counter of an EXISTING session is kept:
// only a human message re-arms those.
func (a *arbiter) dropIfIdle(sid string, now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.bySession[sid]; s != nil && s.idle(now) {
		delete(a.bySession, sid)
	}
}

// idle reports whether the entry holds nothing worth keeping at reading now.
func (s *arbiterState) idle(now time.Time) bool {
	pauseOver := s.gate.RetryAt.IsZero() || !s.gate.RetryAt.After(now)
	return s.holds == 0 && !s.suspended && s.autoResumes == 0 &&
		len(s.overCap) == 0 && s.chainLinks == 0 && len(s.chainClaims) == 0 &&
		!s.sleepAll &&
		pauseOver && s.gate.FreeStreak == 0 && s.gate.PaidStreak == 0
}

// overCapCount returns how many over-cap completion rows sid remembers.
func (a *arbiter) overCapCount(sid string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return 0
	}
	return len(s.overCap)
}

// dropSession deletes sid's entry entirely (the sweep of a deleted session:
// the only moment cap/suspension state stops meaning anything without a human
// message).
func (a *arbiter) dropSession(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.bySession, sid)
}
