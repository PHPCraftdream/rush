// The arbiter's launcher property (docs/plans/2026-10-01-turn-arbiter.md
// sec.4): every verdict that leaves debt open is executed by exactly one
// launcher, and every VDefer with debt guarantees at least one scheduled
// wake. Model-based: the real decide/decideAccount over a tiny in-memory
// model of (session, debt, gate, recheck set, in-flight launch), no provider.
// The mutant flag mirrors the review's revert-check: dropping the paced
// branch's recheck scheduling (today: wakeSession's addToRecheckSet) must
// turn invariant 1 red.
package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type arbModelSession struct {
	debt      bool
	gate      GateFacts
	held      bool
	suspended bool
	inFlight  int // submitted, unconsumed drains
	recheck   bool
	paid      bool // last pacing followed a paid failure (HintOpens=false)
	chain     int
}

type arbModel struct {
	t        *testing.T
	sessions map[string]*arbModelSession
	now      time.Time
	// mutant drops the paced defer's recheck scheduling (the plan's
	// revert-check: the addToRecheckSet of wakeSession's paced branch and of
	// the accounting's paceUnreacted -- the same scheduling class).
	mutant    bool
	violation string
	runs      int
}

func newArbModel(mutant bool) *arbModel {
	return &arbModel{t: nil, sessions: map[string]*arbModelSession{}, now: time.Unix(0, 0), mutant: mutant}
}

func (m *arbModel) sess(id string) *arbModelSession {
	s := m.sessions[id]
	if s == nil {
		s = &arbModelSession{}
		m.sessions[id] = s
	}
	return s
}

func (m *arbModel) failf(format string, args ...any) {
	if m.violation == "" {
		m.violation = fmt.Sprintf(format, args...)
	}
}

// wake models wakeSession/at a site: the real decide over the model's facts,
// launchers executing the verdict exactly as the plan's launcher table says.
func (m *arbModel) wake(id string, site LaunchSite) {
	s := m.sess(id)
	if !s.debt {
		return
	}
	f := TurnFacts{Now: m.now, Site: site, Debt: DebtFacts{PendingIncl: s.debt}, Gate: s.gate}
	f.Session.Held = s.held
	f.Session.Suspended = s.suspended
	f.Session.ChainLinks = s.chain
	f.Session.ChainOwnsAllDebt = s.chain >= turnReactionChainLimit
	v := decide(f)
	switch v.Kind {
	case VRun:
		require.LessOrEqual(m.t, s.inFlight, 0, "invariant 2: a second launch for one verdict (%s)", id)
		s.inFlight++
		m.runs++
	case VDefer:
		scheduled := s.inFlight > 0 || s.recheck || s.gate.RetryAt.After(m.now)
		require.True(m.t, scheduled, "invariant 3: a defer with no launcher (%s: %s)", id, v.Reason)
		pacedOrReopen := v.Reason == "retry pause after an unreacted attempt" ||
			v.Reason == "rerun in progress" || v.Reason == "pending debt left behind" ||
			v.Reason == "launch refused"
		if pacedOrReopen {
			if m.mutant {
				m.failf("invariant 1 (mutant): %s paced defer left with no scheduled retry (%s)", id, v.Reason)
			} else {
				s.recheck = true
			}
		}
	}
}

func (m *arbModel) check(id string) {
	s := m.sess(id)
	if !s.debt {
		return
	}
	f := TurnFacts{Now: m.now, Site: siteTick, Debt: DebtFacts{PendingIncl: true}, Gate: s.gate}
	f.Session.Held = s.held
	f.Session.Suspended = s.suspended
	v := decide(f)
	// A scheduled launcher is: an in-flight launch, or the scheduled retry
	// (the recheck set -- in the web process the 60s pass evaluates ONLY its
	// members, so a bare future RetryAt schedules nothing by itself; the
	// CLI's WaitForHint(RetryAt) is the same "scheduled retry" folded into
	// the flag). Defers whose launcher is an EXTERNAL event (a human message
	// for a suspension or a dormant gate, a new fact for a policy refusal)
	// always have one by construction.
	external := s.suspended || v.Reason == "repeated unreacted attempts" ||
		v.Reason == "another process drives the session" ||
		v.Reason == "released delegation child" || v.Reason == reactionChainReason ||
		v.Reason == "background-shell auto-resume cap reached" ||
		v.Reason == "background-shell completion with auto-resume off"
	hasLauncher := s.inFlight > 0 || s.recheck || external
	if v.Kind == VDefer && !hasLauncher {
		m.failf("invariant 1: %s owes debt with no launcher (verdict %s)", id, v.Reason)
	}
}

func (m *arbModel) drainEnd(id string, reacted bool) {
	s := m.sess(id)
	if s.inFlight > 0 {
		s.inFlight--
	}
	if reacted {
		s.debt = false
		s.chain = 0
		s.gate = GateFacts{}
		return
	}
	// A7: a counted unreacted attempt paces at R; a fact does not open early;
	// the accounting never forgets the session (paceUnreacted's recheck).
	s.gate = GateFacts{
		Paced:      true,
		RetryAt:    m.now.Add(turnRetryAfterFailure()),
		PaidStreak: s.gate.PaidStreak + 1,
		FreeStreak: 0,
		HintSeen:   s.gate.HintNow,
	}
	if s.gate.PaidStreak >= turnDormantStreak {
		s.paid = true
	}
	if !m.mutant {
		s.recheck = true
	} else {
		m.failf("invariant 1 (mutant): %s unreacted attempt paced with no scheduled retry", id)
	}
}

func (m *arbModel) tickPass() {
	// The 60s pass advances the clock partway (a RetryAt inside the interval
	// stays shut -- the paced branch's recheck is what keeps the session
	// scheduled for the NEXT pass).
	m.now = m.now.Add(turnRetryAfterFailure() / 2)
	for id, s := range m.sessions {
		if s.debt && s.recheck {
			s.recheck = false
			m.wake(id, siteTick)
		}
	}
}

func TestArbiterLauncherProperty_NoDanglingDebt(t *testing.T) {
	m := newArbModel(false)
	m.t = t
	const id = "prop-1"
	s := m.sess(id)
	s.debt = true
	// The fact launches a drain; the drain stalls unreacted; the gate paces.
	m.wake(id, siteFact)
	require.Equal(t, 1, m.runs, "the fact's verdict runs")
	m.check(id)
	m.drainEnd(id, false)
	m.check(id)
	// The paced session sits in the recheck set: the tick re-evaluates it
	// while the pause is still running (paced, rescheduled), and the next
	// tick, after the pause, relaunches it.
	require.True(t, s.recheck, "a paced defer is never forgotten")
	m.tickPass()
	require.True(t, s.recheck, "the tick reschedules a still-paced session")
	m.tickPass() // the pause has passed: the tick relaunches
	require.Equal(t, 2, m.runs, "the tick relaunches the paced debt")
	// The reaction closes the debt: no launcher may remain.
	m.drainEnd(id, true)
	m.check(id)
	require.Zero(t, s.inFlight)
}

// TestArbiterLauncherProperty_MutantNoPacedRecheck is the plan's revert-check:
// without the paced defer's recheck scheduling, "the tick missed, the fact
// never came" leaves debt with no launcher -- invariant 1 goes red.
func TestArbiterLauncherProperty_MutantNoPacedRecheck(t *testing.T) {
	m := newArbModel(true)
	m.t = t
	const id = "mutant-1"
	s := m.sess(id)
	s.debt = true
	m.wake(id, siteFact)
	m.drainEnd(id, false)
	m.tickPass() // the tick finds the gate still shut; the mutant drops the reschedule
	require.False(t, s.recheck, "the mutant dropped the scheduling")
	m.now = m.now.Add(turnRetryAfterFailure()) // the pause elapses; no tick fires
	m.check(id)
	require.NotEmpty(t, m.violation, "the mutant must dangle the debt (invariant 1 red)")
	require.Contains(t, m.violation, "invariant 1")
}
