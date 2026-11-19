// The arbiter's mutable state (docs/plans/
// 2026-10-01-turn-arbiter.md sec.2.3, R-ARB-2): ONE struct per session under
// ONE mutex, aggregating what used to be the coordinator maps (auto-resume
// counter, over-cap set, suspension set, rerun holds, the reaction-chain
// trio) plus the launch gate (originally the workLedger's; rehomed with the
// arbiter). Writers: accountDrainAttempt, noteDrainRefused, the human-message
// reset, HoldAutomaticTurns, suspendAutoResume, the reaction chain accounting
// and the bg-shell arrival. Readers go through Arbiter.Snapshot -- one
// capture, copied to a value (never two owner locks at once).
package drainctl

import (
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
)

// Pace says what a pacing counts toward the dormancy streaks (the arbiter's
// gate keeps the same accounting the old ledger gate had).
type Pace uint8

const (
	// PaceUncounted: a refusal or a failed ordinary turn -- pause only.
	PaceUncounted Pace = iota
	// PaceFreeNoTurn: a no-turn Drain left pending debt behind (pull failing).
	PaceFreeNoTurn
	// PacePaidUnreacted: a paid attempt left the debt unreacted.
	PacePaidUnreacted
)

// Ненулевой размер гарантирует отдельную identity для каждой установки.
type AwaitTasksEra struct{ marker byte }

// arbiterState is one session's launch-decision state. Gate.RetryAt zero
// means the gate is open (Paced is derived on the snapshot).
type arbiterState struct {
	holds       int // nested rerun holds (HoldAutomaticTurns)
	suspended   bool
	autoResumes int
	overCap     map[int64]struct{}
	chainLinks  int
	chainClaims map[string]struct{}
	// chainBatchRoots is the batch twin of chainClaims: the root
	// tool_call_ids of the pure-wait batch launches the recorded legs
	// started (each launched root's selected completion becomes a node debt
	// row keyed by the root id).
	chainBatchRoots map[string]struct{}
	chainNoticed    bool
	chainLastLaunch string
	// sleepAll is await_tasks' "until: all" sleep (#1270): armed by the
	// tool, cleared by every allowed drain launch, a human message and
	// Stop. maxWaitWakeID independently remembers either mode's pending once
	// schedule so clear paths can cancel it even when sleepAll is false.
	sleepAll      bool
	maxWaitWakeID string
	awaitEra      *AwaitTasksEra
	gate          GateFacts
}

// Arbiter owns every arbiterState. The zero value is ready to use.
// Clock is installed before use; nil uses wall time.
type Arbiter struct {
	mu        sync.Mutex
	bySession map[string]*arbiterState
	clock     func() time.Time
}

// SetClockForTest installs an injectable clock for tests. It is a no-op
// outside a test binary (guard does not change the call timing: Now keeps
// its nil fallback to wall time).
func (a *Arbiter) SetClockForTest(fn func() time.Time) {
	if !testing.Testing() {
		return
	}
	a.clock = fn
}

// StateView is SeedStateForTest's projection: only the fields the test seam
// may seed.
type StateView struct {
	Gate            GateFacts
	AutoResumes     int
	OverCap         map[int64]struct{}
	ChainLinks      int
	ChainClaims     map[string]struct{}
	ChainBatchRoots map[string]struct{}
	ChainNoticed    bool
}

// SeedStateForTest runs fn over sessionID's state under the arbiter's lock
// and writes back only the projected fields.
func (a *Arbiter) SeedStateForTest(sid string, fn func(*StateView)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	v := &StateView{
		Gate:            s.gate,
		AutoResumes:     s.autoResumes,
		OverCap:         s.overCap,
		ChainLinks:      s.chainLinks,
		ChainClaims:     s.chainClaims,
		ChainBatchRoots: s.chainBatchRoots,
		ChainNoticed:    s.chainNoticed,
	}
	fn(v)
	s.gate = v.Gate
	s.autoResumes = v.AutoResumes
	s.overCap = v.OverCap
	s.chainLinks = v.ChainLinks
	s.chainClaims = v.ChainClaims
	s.chainBatchRoots = v.ChainBatchRoots
	s.chainNoticed = v.ChainNoticed
}

func (a *Arbiter) Now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

func (a *Arbiter) StateIdle(sid string, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	return s == nil || s.idle(now)
}

func (a *Arbiter) stateLocked(sid string) *arbiterState {
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

// ArbiterSnapshot is the value readTurnFacts' in-process half is built from.
// The sets are copied: the caller compares them against DB reads outside the
// lock.
type ArbiterSnapshot struct {
	Held            bool
	Suspended       bool
	AutoResumes     int
	OverCap         map[int64]struct{}
	ChainLinks      int
	ChainClaims     map[string]struct{}
	ChainBatchRoots map[string]struct{}
	ChainNoticed    bool
	SleepAll        bool
	Gate            GateFacts
}

// Snapshot copies sid's state under ONE lock capture.
func (a *Arbiter) Snapshot(sid string) ArbiterSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return ArbiterSnapshot{}
	}
	out := ArbiterSnapshot{
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
	if len(s.chainBatchRoots) > 0 {
		out.ChainBatchRoots = make(map[string]struct{}, len(s.chainBatchRoots))
		for root := range s.chainBatchRoots {
			out.ChainBatchRoots[root] = struct{}{}
		}
	}
	return out
}

// Hold takes a nested rerun hold; the returned func releases it (idempotent
// use of the returned func is the caller's business, as before).
func (a *Arbiter) Hold(sid string) (release func()) {
	a.mu.Lock()
	a.stateLocked(sid).holds++
	a.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			if s := a.bySession[sid]; s != nil && s.holds > 0 {
				s.holds--
				if s.holds == 0 && s.idle(a.Now()) {
					delete(a.bySession, sid)
				}
			}
			a.mu.Unlock()
		})
	}
}

func (a *Arbiter) Held(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	return s != nil && s.holds > 0
}

func (a *Arbiter) Suspend(sid string) {
	a.mu.Lock()
	a.stateLocked(sid).suspended = true
	a.mu.Unlock()
}

func (a *Arbiter) Suspended(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	return s != nil && s.suspended
}

// SetSleepAll arms await_tasks' "until: all" sleep independently of max_wait.
func (a *Arbiter) SetSleepAll(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stateLocked(sid).sleepAll = true
}

// SetMaxWaitWake remembers either await mode's timer without arming sleepAll.
func (a *Arbiter) SetMaxWaitWake(sid, wakeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stateLocked(sid).maxWaitWakeID = wakeID
}

// ClearSleepAll disarms the sleep and returns the pending max_wait schedule
// id ("" when there is none) for the caller to cancel.
func (a *Arbiter) ClearSleepAll(sid string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return ""
	}
	id := s.maxWaitWakeID
	s.awaitEra = nil
	s.sleepAll = false
	s.maxWaitWakeID = ""
	return id
}

// AutoResumesOf returns the bg-shell auto-resume counter since the last human
// message.
func (a *Arbiter) AutoResumesOf(sid string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return 0
	}
	return s.autoResumes
}

// ClaimAutoResumeSlot spends one bg-shell auto-resume slot at a completion's
// admission: eligible is the caller's policy answer (persistent web
// coordinator with AutoResumeOnJobDone on); a completion refused for a spent
// cap is recorded by its notice row id (rowID 0: the insert failed, nothing
// exists to defer). A Stop-suspended session spends nothing. This is THE one
// writer of the cap counter (R2B-16).
func (a *Arbiter) ClaimAutoResumeSlot(sid string, rowID int64, eligible bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	if s.autoResumes >= AutoResumeCap {
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
func (a *Arbiter) pruneOverCapLocked(sid string, notices []sessionnotice.PendingNoticeDebt) {
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
	if s.idle(a.Now()) {
		delete(a.bySession, sid)
	}
}

// PruneOverCap is the best-effort prune at a completion's arrival.
func (a *Arbiter) PruneOverCap(sid string, notices []sessionnotice.PendingNoticeDebt) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneOverCapLocked(sid, notices)
}

// Pace shuts sid's gate for wait (the old gate pacing). It reports the
// moment a streak reached dormancy; kind says which streak the outcome counts
// toward (a paid attempt proves the pull works again, so it clears the free
// streak).
func (a *Arbiter) Pace(sid string, hintAt uint64, wait time.Duration, hintOpens bool, kind Pace) (becameDormant bool) {
	if sid == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g := &a.stateLocked(sid).gate
	g.RetryAt = a.Now().Add(wait)
	g.HintSeen = hintAt
	g.HintOpens = hintOpens
	switch kind {
	case PaceFreeNoTurn:
		g.FreeStreak++
		return g.FreeStreak == DormantStreak
	case PacePaidUnreacted:
		g.FreeStreak = 0
		g.PaidStreak++
		return g.PaidStreak == DormantStreak
	}
	return false
}

// ResetGate opens sid's gate and clears its streaks.
func (a *Arbiter) ResetGate(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.bySession[sid]; s != nil {
		s.gate = GateFacts{}
	}
}

// ResetForHumanMessage clears EVERYTHING one human message re-arms: the
// bg-shell cap and its over-cap set, Stop's suspension, the reaction chain,
// await_tasks' sleep flag with its remembered schedule id, and the launch
// gate. A rerun's hold is NOT a human message's to lift: the holds survive
// the reset and the entry dies only when the last hold is released (P1-2 of
// the R-ARB-2 review).
func (a *Arbiter) ResetForHumanMessage(sid string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return ""
	}
	id := s.maxWaitWakeID
	holds := s.holds
	*s = arbiterState{holds: holds}
	if holds == 0 {
		delete(a.bySession, sid)
	}
	return id
}

// ChainLink accounts one finished Drain leg (the old per-leg link accounting):
// progress resets; self-sustaining idle completions increment; idle launches
// answering another fact start a fresh chain at 1; anything else leaves the
// count as it is. A successful reaction deliberately does NOT reset the count
// -- closing the chain's own completions is the event that masks the chain.
func (a *Arbiter) ChainLink(sid string, progress bool, idleClaims, batchRoots, debtCallIDs []string, snapshot session.BatchDebtSnapshot) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	if progress {
		last := s.chainLastLaunch
		a.resetChainLocked(sid, s)
		s.chainLastLaunch = last
		return
	}
	if len(idleClaims) == 0 && len(batchRoots) == 0 {
		return // a text-only or neutral-tool leg breaks nothing and adds nothing
	}
	// Batch: the chain's own completions are keyed by the launched ROOT
	// tool_call_ids. The debt rows compared are the leg's OWN acted-on rows
	// resolved to their ROOT tool_call_ids (debtCallIDs, computed by the
	// accounting): every row must be a completion of a root the chain
	// launched.
	roots := s.chainBatchRoots
	if roots == nil {
		roots = make(map[string]struct{}, len(batchRoots))
		s.chainBatchRoots = roots
	}
	selfSustaining := true
	for _, call := range debtCallIDs {
		if _, ok := roots[call]; !ok {
			selfSustaining = false
			break
		}
	}
	if len(snapshot.Notices) > 0 {
		selfSustaining = false // another fact (bg-shell done, supervision, ...)
	}
	if selfSustaining {
		s.chainLinks++
	} else {
		s.chainLinks = 1
	}
	// A recorded launch must survive this launch-only leg so the next leg can
	// compare its command. A launch mixed with real action is not a chain link.
	for _, root := range batchRoots {
		roots[root] = struct{}{}
	}
}

func (a *Arbiter) resetChainLocked(sid string, s *arbiterState) {
	s.chainLinks = 0
	s.chainClaims = nil
	s.chainBatchRoots = nil
	s.chainNoticed = false
	s.chainLastLaunch = ""
}

// ChainLastLaunch reports whether command matches the immediately preceding
// async command launch in the session and records this launch under a.mu.
func (a *Arbiter) ChainLastLaunch(sid, command string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	repeated := command != "" && s.chainLastLaunch == command
	s.chainLastLaunch = command
	return repeated
}

// ResetChain clears the chain state (a human message, real progress).
func (a *Arbiter) ResetChain(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.bySession[sid]; s != nil {
		a.resetChainLocked(sid, s)
	}
}

// ChainLinksOf returns the current link count.
func (a *Arbiter) ChainLinksOf(sid string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return 0
	}
	return s.chainLinks
}

// ChainHasClaim reports whether claim belongs to the session's current
// idle-launch chain.
func (a *Arbiter) ChainHasClaim(sid, claim string) bool {
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

// MarkChainNoticed records that the guard warned about sid; it reports
// whether it was ALREADY noticed (the caller inserts the marker only on the
// first hit).
func (a *Arbiter) MarkChainNoticed(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	if s.chainNoticed {
		return true
	}
	s.chainNoticed = true
	return false
}

// SessionsWithState lists the sessions the arbiter holds any state for (the
// sweep's candidates).
func (a *Arbiter) SessionsWithState() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.bySession))
	for id := range a.bySession {
		out = append(out, id)
	}
	return out
}

// DropIfIdle deletes sid's entry when nothing in it means anything any more
// (no hold, no suspension, no cap counter, no chain, no await_tasks sleep,
// and a gate that is open:
// never paced, or its pause has passed with both streaks at zero -- R4B-4).
// A suspended session or a spent cap counter of an EXISTING session is kept:
// only a human message re-arms those.
func (a *Arbiter) DropIfIdle(sid string, now time.Time) {
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
		len(s.chainBatchRoots) == 0 &&
		!s.sleepAll && s.maxWaitWakeID == "" && s.awaitEra == nil &&
		pauseOver && s.gate.FreeStreak == 0 && s.gate.PaidStreak == 0
}

// OverCapCount returns how many over-cap completion rows sid remembers.
func (a *Arbiter) OverCapCount(sid string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil {
		return 0
	}
	return len(s.overCap)
}

// GateHoldsHint reports whether sid's arbiter entry carries a launch gate
// with recorded hint state (HintSeen set or HintOpens): while it does,
// dropping the coordinator's hint counter would desynchronize gateShut's
// newer-hint comparison -- a counter that restarts at zero reads as "any
// inequality = a newer fact" and reopens a dormant gate with no fact at all.
func (a *Arbiter) GateHoldsHint(sid string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	return s != nil && (s.gate.HintSeen != 0 || s.gate.HintOpens)
}

// DropSession deletes sid's entry entirely (the sweep of a deleted session:
// the only moment cap/suspension state stops meaning anything without a human
// message).
func (a *Arbiter) DropSession(sid string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.bySession, sid)
}

// BeginAwait starts a new await era.
func (a *Arbiter) BeginAwait(sid string, all bool) (*AwaitTasksEra, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.stateLocked(sid)
	if s.suspended {
		return nil, ""
	}
	old := s.maxWaitWakeID
	era := &AwaitTasksEra{}
	s.awaitEra, s.sleepAll, s.maxWaitWakeID = era, all, ""
	return era, old
}

// InstallAwait accepts the timer created during the era.
func (a *Arbiter) InstallAwait(sid string, era *AwaitTasksEra, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.bySession[sid]
	if s == nil || era == nil || s.awaitEra != era || s.suspended {
		return false
	}
	s.maxWaitWakeID = id
	return true
}

// AbortAwait cancels the era.
func (a *Arbiter) AbortAwait(sid string, era *AwaitTasksEra) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.bySession[sid]; s != nil && s.awaitEra == era {
		s.awaitEra, s.sleepAll, s.maxWaitWakeID = nil, false, ""
	}
}
