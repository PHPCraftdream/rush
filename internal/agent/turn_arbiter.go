// The turn arbiter (R-ARB-1, docs/plans/2026-10-01-turn-arbiter.md): TurnFacts,
// Verdict and the pure decide function. This step does NOT move callers:
// drainPermitted/decideDrainTurn/CLIScope stay the running code (R-ARB-2
// translates them); readTurnFacts is the shadow's fact collector.
package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// LaunchSite names where a launch decision is asked from. The site is part of
// the facts: only a fact (siteFact) spends a bg-shell auto-resume slot, only
// the accounting side (siteAccount) may answer VClose.
type LaunchSite uint8

const (
	siteFact LaunchSite = iota
	siteRelease
	siteTick
	siteCommit
	siteCLI
	// siteChild is a parked delegation child's re-check launch
	// (workLedger.recheckChild): a release-shaped retry that spends no slot.
	siteChild
	// siteAccount is the accounting side (accountDrainAttempt's rules A1-A7).
	// Close verdicts are only ever issued here, never on a launch site.
	siteAccount
)

// The arbiter's named constants: K closes a debt row by its own counted
// attempts, N bounds the idle reaction chain, D is either dormancy streak of
// the launch gate, CAP bounds bg-shell auto-resumes per human message and R
// paces the next launch after an unreacted attempt. Re-exported from their
// owning files so the values never diverge.
const (
	turnFailureSettleThreshold = drainFailureSettleThreshold // K
	turnReactionChainLimit     = ReactionChainLimit          // N
	turnDormantStreak          = drainDormantStreak          // D
	turnAutoResumeCap          = maxConsecutiveAutoResumes   // CAP
)

// turnRetryAfterFailure is R: the launch-gate pause after an unreacted
// attempt.
func turnRetryAfterFailure() time.Duration { return drainRetryAfterFailure() }

// foreignWorkspaceReason is the arbiter's WS-1 verdict text (#1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md §1 "drain/reaction"). It is
// deliberately NOT one of the paced reasons and NOT "another process drives
// the session": a session this process has no claim on is not worth a
// recheck, because the process that OWNS it reacts to that debt in its own
// checkout -- drainVerdictOf must therefore leave it deferred without one.
const foreignWorkspaceReason = "session belongs to another workspace"

// SessionFacts is the in-process (coordinator + workLedger) half of the
// snapshot.
type SessionFacts struct {
	ExternallyDrivenSelf bool
	// ForeignWorkspace is the WS-1 refusal (#1142 step C): the session this
	// decision is about belongs to a DIFFERENT workspace, so this process may
	// not drive it at all -- neither the launch side may start a turn for it,
	// nor the accounting side may count, pace or settle anything on it.
	// Stamped from the run/turn error that carried the refusal; see
	// readTurnFacts and foreignWorkspaceReason.
	ForeignWorkspace  bool
	ForeignDriverLive bool
	Held              bool
	Suspended         bool
	ChainLinks        int
	ChainOwnsAllDebt  bool
	ChainNoticed      bool
	RunningDelegation bool
	DurableChild      bool
	AutonomyEnabled   bool
	AutoResumes       int
}

// DebtFacts is the DB half of the snapshot. OverCapRows counts the debt's
// bg-shell completion rows that arrived with every auto-resume slot spent;
// BGShellNotices counts the debt's notice rows in total, so "OverCapRows
// covers the whole bg-shell debt" is BGShellOnly && OverCapRows ==
// BGShellNotices (the same predicate the arbiter cap rule answers).
type DebtFacts struct {
	Visible        session.DebtSnapshot
	PendingIncl    bool
	BGShellOnly    bool
	BGShellNotices int
	OverCapRows    int
}

// GateFacts is the per-session launch gate, copied under l.mu.
type GateFacts struct {
	Paced      bool
	RetryAt    time.Time
	HintOpens  bool
	HintSeen   uint64
	HintNow    uint64
	FreeStreak int
	PaidStreak int
}

// AttemptFacts carries one Drain leg's outcome for the accounting side
// (decideAccount). Zero for every launch site.
type AttemptFacts struct {
	// NotAttempted: the leg never reached the provider (a refusal or a
	// preamble failure); a cancelled preamble counts nothing at all (A1's
	// own first branch). Refused is NotAttempted with a non-cancel cause.
	NotAttempted bool
	Refused      bool
	// NoTurn: admitted but the commit decision said no. PendingLeft says
	// pending-inclusive debt remains after it (the pull keeps failing).
	// CommitRefused says the commit was refused while visible debt stays
	// (a paced/stuck gate, a hold, a suspension, the chain guard, the cap,
	// a foreign driver, an unreadable input): the launch gate is the launch
	// side's own state, so the accounting leaves it alone (A3').
	NoTurn        bool
	PendingLeft   bool
	CommitRefused bool
	// Exempt: the leg ended for a reason that is not evidence about the
	// debt (operator stop).
	Exempt bool
	// Terminal: a provider failure no retry can fix.
	Terminal bool
	// MaxWakeAttempts is the snapshot's highest per-row attempt counter
	// after the attempt was counted; 0 means every visible row reacted.
	MaxWakeAttempts int
	// OpenRows is how many snapshot rows are still debt; AtK is the rows
	// whose own counter reached K.
	OpenRows int
	AtK      session.DebtSnapshot
}

// TurnFacts is one snapshot of every input the launch decision reads
// (design sec.2.1). It is a value: decide never touches the owners.
type TurnFacts struct {
	Now      time.Time
	Site     LaunchSite
	Session  SessionFacts
	Debt     DebtFacts
	Gate     GateFacts
	Attempt  AttemptFacts
	Snapshot session.DebtSnapshot
}

// VerdictKind is the outcome of decide.
type VerdictKind uint8

const (
	// VRun: submit the Drain turn. Counted marks that this launch spends a
	// bg-shell auto-resume slot (only a fact does).
	VRun VerdictKind = iota
	// VDefer: the debt stays; the launch is retried later.
	VDefer
	// VClose: settle the rows by failure (accounting side only).
	VClose
	// VNone: nothing to decide (no debt, exempt, gate reset).
	VNone
)

// Verdict is one launch or accounting decision (design sec.2.2).
type Verdict struct {
	Kind VerdictKind
	// Counted is descriptive: it repeats which site the verdict came from
	// (rule 13 -- only siteFact). The bg-shell slot itself is spent at the
	// completion's arrival (claimAutoResumeSlot), never by executing a
	// VRun; no executor reads this field.
	Counted bool
	Reason  string
	// RecheckAt is zero for a defer the tick or a release retries (a hold),
	// non-zero for a gate pause.
	RecheckAt time.Time
	// ReopenOnHint: a newer fact hint may open the gate before RecheckAt.
	ReopenOnHint bool
	// Rows names what VClose settles; Marker is the wake_failed text.
	Rows   session.DebtSnapshot
	Marker string
}

// gateShut reports the launch-gate half of the table (rules 8-10): the gate
// verdict for f's gate state, or ok=false when the gate is open and the
// decision falls through to the policy rows.
func gateShut(f TurnFacts) (v Verdict, ok bool) {
	paidDormant := f.Gate.PaidStreak >= turnDormantStreak
	freeDormant := f.Gate.FreeStreak >= turnDormantStreak
	hintOpens := f.Gate.HintOpens && f.Gate.HintNow != f.Gate.HintSeen && !paidDormant
	if paidDormant {
		// Rule 8: only a human message (or a restart) reopens a paid-dormant
		// gate; a newer fact does not (R3B-4).
		return Verdict{Kind: VDefer, Reason: "repeated unreacted attempts", RecheckAt: f.Gate.RetryAt}, true
	}
	if freeDormant {
		// Rule 10: like rule 8, but a newer fact reopens a free-dormant gate.
		if hintOpens {
			return Verdict{}, false
		}
		return Verdict{Kind: VDefer, Reason: "repeated unreacted attempts", RecheckAt: f.Gate.RetryAt, ReopenOnHint: true}, true
	}
	if f.Gate.Paced && f.Gate.RetryAt.After(f.Now) {
		// Rule 9: inside the pause a newer fact opens early (unless the pause
		// followed a paid failure, excluded above).
		if hintOpens {
			return Verdict{}, false
		}
		return Verdict{Kind: VDefer, Reason: "retry pause after an unreacted attempt", RecheckAt: f.Gate.RetryAt, ReopenOnHint: f.Gate.HintOpens}, true
	}
	return Verdict{}, false
}

// decide is THE pure launch decision (design sec.3, with the orchestrator's
// corrected order, sec.9.1): WS-1, then rules 1-5 and 7, then the gate (8-10)
// for every session including a child under a running delegation, then 11-12,
// which a running delegation skips, then 13. No clocks (Now is input), no I/O,
// no global state. The rule order is contract: swapping 5/6 or 6/8 must turn
// the table test red.
func decide(f TurnFacts) Verdict {
	if f.Site == siteAccount {
		return decideAccount(f)
	}
	// WS-1 (#1142 step C, docs/plans/2026-10-01-shared-data-dir.md §1
	// "drain/reaction"): the session belongs to another workspace, so its
	// debt is not this process's to react to -- the owning checkout runs the
	// reaction there. Checked before every other rule: ownership is the
	// precondition for having an opinion about the debt at all. No pacing (the
	// gate is ours, the session is not) and no recheck (the reason is not
	// transient), which is what distinguishes it from rule 2.
	if f.Session.ForeignWorkspace {
		return Verdict{Kind: VDefer, Reason: foreignWorkspaceReason}
	}
	// 1: no pending-inclusive debt -- nothing to launch for.
	if !f.Debt.PendingIncl {
		return Verdict{Kind: VNone, Reason: "no debt"}
	}
	// 2: another live process drives this session (DUR-10).
	if f.Session.ForeignDriverLive && !f.Session.ExternallyDrivenSelf {
		return Verdict{Kind: VDefer, Reason: "another process drives the session"}
	}
	// 3: a rerun holds the session; temporary, so paced with no clock (R3C-5).
	if f.Session.Held {
		return Verdict{Kind: VDefer, Reason: "rerun in progress", ReopenOnHint: true}
	}
	// 4: Stop or a pending question suspended automatic turns.
	if f.Session.Suspended {
		return Verdict{Kind: VDefer, Reason: "automatic turns suspended"}
	}
	// 5: the reaction chain guard (#1113) -- deliberately ABOVE the running
	// delegation shortcut it replaced (R6B-1).
	if f.Session.ChainLinks >= turnReactionChainLimit && f.Session.ChainOwnsAllDebt {
		return Verdict{Kind: VDefer, Reason: reactionChainReason}
	}
	// 7: a released or expired delegation child never falls through to the
	// root agent (B3/C6) -- but only when its delegation is NOT running:
	// a child whose delegation row runs is driven by that delegation.
	if f.Session.DurableChild && !f.Session.RunningDelegation && !f.Session.ExternallyDrivenSelf {
		return Verdict{Kind: VDefer, Reason: "released delegation child"}
	}
	// 8-10: the per-session launch gate, for EVERY session (sec.9.1: today
	// drainPermitted applies it after any allow, a child under a running
	// delegation included).
	if v, shut := gateShut(f); shut {
		return v
	}
	if !f.Session.RunningDelegation {
		// 11: bg-shell auto-resume cap (R3B-6, amendment (aa)): a launch
		// that spends no slot never acts on a debt every row of which is an
		// over-cap completion.
		if f.Session.AutonomyEnabled && noSlotSite(f.Site) &&
			f.Debt.BGShellOnly && f.Debt.OverCapRows > 0 &&
			f.Debt.OverCapRows == f.Debt.BGShellNotices {
			return Verdict{Kind: VDefer, Reason: "background-shell auto-resume cap reached"}
		}
		// 12: background shell with auto-resume off.
		if !f.Session.AutonomyEnabled && f.Debt.BGShellOnly {
			return Verdict{Kind: VDefer, Reason: "background-shell completion with auto-resume off"}
		}
	}
	// 13.
	return Verdict{Kind: VRun, Counted: f.Site == siteFact}
}

// noSlotSite reports the launch sites whose decision never spends a
// bg-shell auto-resume slot (only a fact does).
func noSlotSite(s LaunchSite) bool {
	switch s {
	case siteTick, siteRelease, siteCommit, siteCLI, siteChild:
		return true
	}
	return false
}

// decideAccount is the accounting side (design sec.3, rows A1-A7): the
// verdict accountDrainAttempt executes after a Drain leg ends. Inputs are
// the leg's AttemptFacts and the same session/debt/gate snapshot; the
// streak and gate writes themselves stay with the accounting (they are the
// state, not the decision).
func decideAccount(f TurnFacts) Verdict {
	// WS-1 (#1142 step C): a leg that was refused because the session belongs
	// to another workspace is not evidence about the debt -- the debt is
	// answered in its owner's checkout. It is also not a "launch refusal"
	// worth pacing our gate for, so it never reaches A1.
	if f.Session.ForeignWorkspace {
		return Verdict{Kind: VDefer, Reason: foreignWorkspaceReason}
	}
	// A1: the attempt never reached the provider. A cancelled preamble is
	// nothing at all; a refusal paces the launch (not counted, the debt
	// stays) -- for a session this process's own rush run loop drives, at
	// the loop's 0.5s cadence, otherwise at R.
	if f.Attempt.NotAttempted {
		if f.Attempt.Refused {
			wait := turnRetryAfterFailure()
			if f.Session.ExternallyDrivenSelf {
				wait = drainRefusalPauseLoop()
			}
			return Verdict{Kind: VDefer, Reason: "launch refused", RecheckAt: f.Now.Add(wait), ReopenOnHint: true}
		}
		return Verdict{Kind: VNone, Reason: "cancelled preamble"}
	}
	// A2/A3/A3': a no-turn Drain.
	if f.Attempt.NoTurn {
		if f.Attempt.CommitRefused {
			// A3': the launch verdict already decided the gate; the attempt
			// is no evidence about it. The executor only carries the
			// recheck question (the old default branch).
			return Verdict{Kind: VNone, Reason: "commit refused: gate untouched"}
		}
		if f.Attempt.PendingLeft {
			return Verdict{Kind: VDefer, Reason: "pending debt left behind", RecheckAt: f.Now.Add(turnRetryAfterFailure()), ReopenOnHint: true}
		}
		return Verdict{Kind: VNone, Reason: "no debt: gate reset"}
	}
	// A4: an exempt attempt (operator stop) is no evidence about the debt.
	if f.Attempt.Exempt {
		return Verdict{Kind: VNone, Reason: "operator stop: not counted"}
	}
	// A5: every visible row reacted -- the gate resets, the chain count too.
	if f.Attempt.MaxWakeAttempts == 0 {
		return Verdict{Kind: VNone, Reason: "all rows reacted: gate reset"}
	}
	// A6: a terminal provider failure closes every row the attempt saw at
	// once; otherwise a row whose OWN counter reached K is closed by failure
	// (a notice that arrived while earlier ones were failing keeps its own
	// clock -- a one-minute outage never closes it). The accounting settles
	// exactly the named rows.
	if f.Attempt.Terminal {
		return Verdict{Kind: VClose, Rows: f.Snapshot, Reason: "terminal provider failure"}
	}
	if len(f.Attempt.AtK.Jobs)+len(f.Attempt.AtK.Notices) > 0 {
		return Verdict{Kind: VClose, Rows: f.Attempt.AtK, Reason: "row reached its attempt limit"}
	}
	// A7: otherwise this was a counted, unreacted attempt: the gate paces at
	// R and a newer fact does NOT open it early (R3B-4).
	return Verdict{Kind: VDefer, Reason: "unreacted attempt", RecheckAt: f.Now.Add(turnRetryAfterFailure())}
}

// readTurnFacts collects the ONE snapshot the arbiter decides on (design
// sec.2.1). Order is law (ASYNC-02/09 anchor): the DB half FIRST, then the
// in-process half -- a fact committed between the reads is already durable
// when the memory half is captured, so it is seen; the inverted order could
// lose it. No DB I/O happens under the arbiter's or the ledger's mutex: each
// is captured once and copied to a value. The bgArrival mutex is taken
// BEFORE the debt-rows read and spans the in-process half -- exactly the
// completion arrival's own span (insert + slot decision under the same
// gate) -- so the debt rows and the over-cap set are ONE consistent pair:
// a completing background shell is either fully visible to both or to
// neither. The read never writes the set; pruning happens only at a
// completion's arrival (pruneBGShellOverCap, under bgArrival). The rows are
// the ONE PendingInclusiveDebtRows read: the debt's existence, summary,
// kinds, over-cap matching and the chain guard's claims all come from it.
// An unreadable input is an error: the caller fails closed.
//
// runErrs is optional and carries the caller's knowledge of this session's
// run/turn leg, which the DB half below cannot see: when a leg was refused
// because the session belongs to another workspace (WS-1, #1142 step C), the
// ForeignWorkspace fact is stamped into the snapshot. Callers with no leg
// error in hand (every launch site, every existing test) pass none and get
// exactly the pre-#1142 snapshot.
func (c *coordinator) readTurnFacts(ctx context.Context, sessionID string, site LaunchSite, runErrs ...error) (TurnFacts, error) {
	f := TurnFacts{Now: time.Now(), Site: site, Snapshot: session.DebtSnapshot{}}
	for _, err := range runErrs {
		if errors.Is(err, session.ErrForeignWorkspace) {
			f.Session.ForeignWorkspace = true
		}
	}
	l := c.asyncJobs
	if l == nil || l.store == nil {
		return f, nil
	}
	// ---- DB half (the debt picture itself is the ONE rows read under
	// bgArrival, below; only the visible snapshot and the session-side
	// markers are read outside the gate) ----
	// NOTE: no short-circuit on no-debt. The facts are ONE snapshot (the
	// ASYNC-02/09 anchor): a caller that re-asks the arbiter without rule
	// 1's short-circuit (drainPolicy preserves the old policy's debt-free
	// refusal) still needs the complete session half -- a foreign driver or
	// a suspension refuses a debt-free session too.
	snap, err := l.store.CaptureDebtSnapshot(ctx, sessionID)
	if err != nil {
		return f, fmt.Errorf("turn facts: debt snapshot: %w", err)
	}
	f.Debt.Visible = snap
	if seam := readTurnFactsGateSeam.Load(); seam != nil {
		(*seam)()
	}
	if err := c.bgArrival.lock(ctx); err != nil {
		return f, fmt.Errorf("turn facts: bg arrival: %w", err)
	}
	defer c.bgArrival.unlock()
	hasJobDebt, notices, jobClaims, err := l.store.PendingInclusiveDebtRows(ctx, sessionID)
	if err != nil {
		return f, fmt.Errorf("turn facts: debt rows: %w", err)
	}
	f.Debt.BGShellOnly = !hasJobDebt && len(notices) > 0
	for _, n := range notices {
		if n.Kind != session.NoticeKindBGShellDone {
			f.Debt.BGShellOnly = false
		}
	}
	f.Debt.BGShellNotices = len(notices)
	// Pending-inclusive debt, from the SAME rows read (same predicate as
	// ReactionDebtExists): a debt that appeared after the earlier reads is
	// still this decision's debt.
	f.Debt.PendingIncl = hasJobDebt || len(notices) > 0
	f.Session.ForeignDriverLive, err = l.foreignLiveDriver(ctx, sessionID)
	if err != nil {
		return f, fmt.Errorf("turn facts: driver marker: %w", err)
	}
	f.Session.RunningDelegation, err = l.hasRunningDelegationFor(ctx, sessionID)
	if err != nil {
		return f, fmt.Errorf("turn facts: delegation state: %w", err)
	}
	if !f.Session.ExternallyDrivenSelf {
		f.Session.DurableChild, err = c.isDurableDelegationChild(ctx, sessionID)
		if err != nil {
			return f, fmt.Errorf("turn facts: delegation identity: %w", err)
		}
	}
	if c.cfg != nil {
		f.Session.AutonomyEnabled = c.autonomyEnabled()
	}
	// ---- in-process half (bgArrival already spans the rows read above) ----
	s := c.arb.snapshot(sessionID)
	owed := make(map[int64]struct{}, len(notices))
	for _, n := range notices {
		owed[n.ID] = struct{}{}
	}
	if s.AutoResumes >= turnAutoResumeCap { // over-cap rows only count with every slot spent
		for id := range s.OverCap {
			if _, ok := owed[id]; ok {
				f.Debt.OverCapRows++
			}
		}
	}
	f.Session.AutoResumes = s.AutoResumes
	f.Session.Held = s.Held
	f.Session.Suspended = s.Suspended
	f.Session.ChainLinks = s.ChainLinks
	f.Session.ChainNoticed = s.ChainNoticed
	// The chain guard's second half: the ENTIRE pending-inclusive debt must
	// be completions of the chain's own idle launches -- no notice row at
	// all and every job claim inside the chain's set.
	if f.Session.ChainLinks >= turnReactionChainLimit && len(notices) == 0 && len(jobClaims) > 0 {
		owns := true
		for _, claim := range jobClaims {
			if _, ok := s.ChainClaims[claim]; !ok {
				owns = false
				break
			}
		}
		f.Session.ChainOwnsAllDebt = owns
	}
	// ---- ledger half (l.mu): hint counter + external driver; the gate
	// itself lives in the arbiter state ----
	f.Gate = s.Gate
	f.Gate.HintNow = l.hintSeqOf(sessionID)
	f.Session.ExternallyDrivenSelf = l.isExternalDriver(sessionID)
	return f, nil
}
