// Package drainctl owns the drain/arbiter guarantee. This file is the pure
// turn-arbiter core (R-ARB-1, docs/plans/2026-10-01-turn-arbiter.md): the
// fact types, the verdict and the pure Decide function.
package drainctl

import (
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// LaunchSite names where a launch decision is asked from. The site is part of
// the facts: only a fact (SiteFact) spends a bg-shell auto-resume slot, only
// the accounting side (SiteAccount) may answer VClose.
type LaunchSite uint8

const (
	SiteFact LaunchSite = iota
	SiteRelease
	SiteTick
	SiteCommit
	SiteCLI
	// SiteChild is a parked delegation child's re-check launch: a
	// release-shaped retry that spends no slot.
	SiteChild
	// SiteAccount is the accounting side (accountDrainAttempt's rules A1-A7).
	// Close verdicts are only ever issued here, never on a launch site.
	SiteAccount
)

// The arbiter's owning constants: K closes a debt row by its own counted
// attempts, N bounds the idle reaction chain, D is either dormancy streak of
// the launch gate and CAP bounds bg-shell auto-resumes per human message.
// Ownership moved here from drain_component.go, reaction_component.go and
// coordinator.go so the values never diverge; the agent package keeps
// same-named aliases for its retained callers and tests.
const (
	FailureSettleThreshold = 3 // K
	ReactionChainLimit     = 3 // N
	DormantStreak          = 3 // D
	AutoResumeCap          = 5 // CAP
)

// ReactionChainReason is the Deferred reason the reaction chain guard
// reports (moved from reaction_component.go).
const ReactionChainReason = "reaction chain without progress"

// ForeignWorkspaceReason is the arbiter's WS-1 verdict text (#1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md §1 "drain/reaction"). It is
// deliberately NOT one of the paced reasons and NOT "another process drives
// the session": a session this process has no claim on is not worth a
// recheck, because the process that OWNS it reacts to that debt in its own
// checkout -- Decide must therefore leave it deferred without one.
const ForeignWorkspaceReason = "session belongs to another workspace"

// SessionFacts is the in-process (coordinator + its rehomed runtime state)
// half of the snapshot.
type SessionFacts struct {
	ExternallyDrivenSelf bool
	// ForeignWorkspace is the WS-1 refusal (#1142 step C): the session this
	// decision is about belongs to a DIFFERENT workspace, so this process may
	// not drive it at all -- neither the launch side may start a turn for it,
	// nor the accounting side may count, pace or settle anything on it.
	// Stamped from the run/turn error that carried the refusal; see
	// readTurnFacts and ForeignWorkspaceReason.
	ForeignWorkspace  bool
	ForeignDriverLive bool
	Held              bool
	Suspended         bool
	ChainLinks        int
	ChainOwnsAllDebt  bool
	ChainNoticed      bool
	// ChainBatchRoots is the ON-path twin of the ledger path's ChainClaims:
	// the root tool_call_ids of the pure-wait batch launches the recorded
	// chain legs started. readTurnFactsBatch's rule-5 predicate compares
	// the debt's node rows against this set (ownership, not over-deferral).
	ChainBatchRoots   []string
	RunningDelegation bool
	// RunningOwnJobs counts the session's running await_tasks work: its own
	// async rows PLUS its live descendant delegation rows (rule 4b; the
	// Child-side RunningDelegation is a different fact). readTurnFacts reads
	// it only while the sleep is armed, so the hot path pays nothing.
	RunningOwnJobs  int
	DurableChild    bool
	AutonomyEnabled bool
	// SleepAll is await_tasks' "until: all" sleep (rule 4b).
	SleepAll    bool
	AutoResumes int
}

// DebtFacts is the DB half of the snapshot. OverCapRows counts the debt's
// bg-shell completion rows that arrived with every auto-resume slot spent;
// BGShellNotices counts the debt's notice rows in total, so "OverCapRows
// covers the whole bg-shell debt" is BGShellOnly && OverCapRows ==
// BGShellNotices (the same predicate the arbiter cap rule answers).
type DebtFacts struct {
	Visible     session.BatchDebtSnapshot
	PendingIncl bool
	BGShellOnly bool
	// CompletionOnly reports whether every pending row is a COMPLETION of
	// finished work (#1270 rule 4b): notice rows count only when their kind
	// is bg_shell_done or supervision, and job-debt rows are treated as
	// completion-class unconditionally -- the rows read here
	// (PendingInclusiveDebtRows) do not carry the job row's kind, and a
	// finished job's pending delivery IS a completion. A pending child
	// question (NoticeKindChildQuestion) is never completion-class, so a
	// sleeping session wakes for it at once.
	CompletionOnly bool
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
// (DecideAccount). Zero for every launch site.
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
	// whose own counter reached K. AtKBatch is the batch-node twin of AtK
	// on the flag-on path; UnmappedAtK counts threshold rows with no node
	// identity.
	OpenRows    int
	AtK         session.BatchDebtSnapshot
	AtKBatch    session.BatchDebtSnapshot
	UnmappedAtK int
}

// TurnFacts is one snapshot of every input the launch decision reads
// (design sec.2.1). It is a value: Decide never touches the owners.
type TurnFacts struct {
	Now      time.Time
	Site     LaunchSite
	Session  SessionFacts
	Debt     DebtFacts
	Gate     GateFacts
	Attempt  AttemptFacts
	Snapshot session.BatchDebtSnapshot
	// BatchSnapshot is the flag-on path's visible batch-node debt (the
	// accounting side's VClose rows); empty on the ledger path.
	BatchSnapshot session.BatchDebtSnapshot
}

// VerdictKind is the outcome of Decide.
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
	// (rule 13 -- only SiteFact). The bg-shell slot itself is spent at the
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
	Rows   session.BatchDebtSnapshot
	Marker string
	// BatchRows is the batch-node twin of Rows on the flag-on path.
	BatchRows session.BatchDebtSnapshot
}

// GateShut reports the launch-gate half of the table (rules 8-10): the gate
// verdict for f's gate state, or ok=false when the gate is open and the
// decision falls through to the policy rows.
func GateShut(f TurnFacts) (v Verdict, ok bool) {
	paidDormant := f.Gate.PaidStreak >= DormantStreak
	freeDormant := f.Gate.FreeStreak >= DormantStreak
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

// Decide is THE pure launch decision (design sec.3, with the orchestrator's
// corrected order, sec.9.1): WS-1, then rules 1-4b and 7, then the gate (8-10)
// for every session including a child under a running delegation, then 11-12,
// which a running delegation skips, then 13. No clocks (Now is input), no I/O,
// no global state. The rule order is contract: swapping 5/6 or 6/8 must turn
// the table test red.
func Decide(f TurnFacts, retryAfterFailure, refusalPauseLoop func() time.Duration) Verdict {
	if f.Site == SiteAccount {
		return DecideAccount(f, retryAfterFailure, refusalPauseLoop)
	}
	// WS-1 (#1142 step C, docs/plans/2026-10-01-shared-data-dir.md §1
	// "drain/reaction"): the session belongs to another workspace, so its
	// debt is not this process's to react to -- the owning checkout runs the
	// reaction there. Checked before every other rule: ownership is the
	// precondition for having an opinion about the debt at all. No pacing (the
	// gate is ours, the session is not) and no recheck (the reason is not
	// transient), which is what distinguishes it from rule 2.
	if f.Session.ForeignWorkspace {
		return Verdict{Kind: VDefer, Reason: ForeignWorkspaceReason}
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
	// 4b (#1270): await_tasks' "until: all" sleep. While the sleep is armed
	// AND the session still has running rows (own jobs or delegations -- an
	// open once wake schedule is NOT work) AND every pending row is a
	// completion of that work, the launch defers: the sleep is broken only
	// by a completion (each one re-wakes the session) or the max_wait
	// schedule. A question debt (or any other non-completion row) makes
	// CompletionOnly false and falls through at once.
	if f.Session.SleepAll && f.Debt.CompletionOnly &&
		(f.Session.RunningOwnJobs > 0 || f.Session.RunningDelegation) {
		return Verdict{Kind: VDefer, Reason: "sleeping until all tasks finish"}
	}
	// 5: the reaction chain guard (#1113) -- deliberately ABOVE the running
	// delegation shortcut it replaced (R6B-1).
	if f.Session.ChainLinks >= ReactionChainLimit && f.Session.ChainOwnsAllDebt {
		return Verdict{Kind: VDefer, Reason: ReactionChainReason}
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
	if v, shut := GateShut(f); shut {
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
	return Verdict{Kind: VRun, Counted: f.Site == SiteFact}
}

// noSlotSite reports the launch sites whose decision never spends a
// bg-shell auto-resume slot (only a fact does).
func noSlotSite(s LaunchSite) bool {
	switch s {
	case SiteTick, SiteRelease, SiteCommit, SiteCLI, SiteChild:
		return true
	}
	return false
}

// DecideAccount is the accounting side (design sec.3, rows A1-A7): the
// verdict accountDrainAttempt executes after a Drain leg ends. Inputs are
// the leg's AttemptFacts and the same session/debt/gate snapshot; the
// streak and gate writes themselves stay with the accounting (they are the
// state, not the decision).
func DecideAccount(f TurnFacts, retryAfterFailure, refusalPauseLoop func() time.Duration) Verdict {
	// WS-1 (#1142 step C): a leg that was refused because the session belongs
	// to another workspace is not evidence about the debt -- the debt is
	// answered in its owner's checkout. It is also not a "launch refusal"
	// worth pacing our gate for, so it never reaches A1.
	if f.Session.ForeignWorkspace {
		return Verdict{Kind: VDefer, Reason: ForeignWorkspaceReason}
	}
	// A1: the attempt never reached the provider. A cancelled preamble is
	// nothing at all; a refusal paces the launch (not counted, the debt
	// stays) -- for a session this process's own rush run loop drives, at
	// the loop's 0.5s cadence, otherwise at R.
	if f.Attempt.NotAttempted {
		if f.Attempt.Refused {
			wait := retryAfterFailure()
			if f.Session.ExternallyDrivenSelf {
				wait = refusalPauseLoop()
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
			return Verdict{Kind: VDefer, Reason: "pending debt left behind", RecheckAt: f.Now.Add(retryAfterFailure()), ReopenOnHint: true}
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
		return Verdict{Kind: VClose, Rows: f.Snapshot, BatchRows: f.BatchSnapshot, Reason: "terminal provider failure"}
	}
	if len(f.Attempt.AtK.Nodes)+len(f.Attempt.AtK.Notices) > 0 {
		return Verdict{Kind: VClose, Rows: f.Attempt.AtK, Reason: "row reached its attempt limit"}
	}
	if len(f.Attempt.AtKBatch.Nodes)+len(f.Attempt.AtKBatch.Notices) > 0 || f.Attempt.UnmappedAtK > 0 {
		return Verdict{Kind: VClose, Rows: f.Attempt.AtK, BatchRows: f.Attempt.AtKBatch, Reason: "row reached its attempt limit"}
	}
	// A7: otherwise this was a counted, unreacted attempt: the gate paces at
	// R and a newer fact does NOT open it early (R3B-4).
	return Verdict{Kind: VDefer, Reason: "unreacted attempt", RecheckAt: f.Now.Add(retryAfterFailure())}
}
