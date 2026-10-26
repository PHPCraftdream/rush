// BatchCutoverFacts is the R12-E single fact-source seam for the job-batches
// cutover: the terminal/debt/scope/reaction/wake questions the pre-cutover
// consumers direct at the async_jobs ledger. The node-backed implementation
// is the only source: it reads batch_nodes/batch outbox/session_notices only
// and never touches async_jobs (the legacy adapter is gone). Wake schedules
// and session driver markers are retained host infrastructure and stay on
// their existing readers.
package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// BatchLiveScopeEntry is one owner's live-work scope answer.
type BatchLiveScopeEntry struct {
	Owner string
	// Live: the owner has live work on a host not provably dead. A failed or
	// incomplete read reports true, never false.
	Live bool
}

// BatchAttemptRows is the post-attempt per-row picture of the visible debt
// (the accounting side's A5/A6 inputs).
type BatchAttemptRows struct {
	// Open counts rows still debt; Max is the highest per-row attempt
	// counter (0 iff Open == 0); AtK holds the rows whose own counter
	// reached the close threshold. UnmappedAtK counts threshold rows with
	// no node identity (async_jobs claims) that AtK cannot name.
	Open        int
	Max         int
	AtK         session.BatchDebtSnapshot
	UnmappedAtK int
}

// BatchCutoverFacts answers the consumers' fact questions. Every method
// applies the conservative rule: a failed or incomplete read reports
// "unknown" as debt/live = true, never as idle.
type BatchCutoverFacts interface {
	// SelectedDebtExists reports finished-but-unreacted (pending-inclusive
	// selected-notice) debt for owner.
	SelectedDebtExists(ctx context.Context, owner string) (bool, error)
	// CaptureVisibleDebt captures the visible (delivery='done') debt
	// snapshot for settle-by-failure accounting.
	CaptureVisibleDebt(ctx context.Context, owner string) (session.BatchDebtSnapshot, error)
	// PendingInclusiveDebt reads the ONE pending-inclusive rows set: node
	// debt plus notice rows with kinds (held questions included).
	PendingInclusiveDebt(ctx context.Context, owner string) (session.BatchPendingDebt, error)
	// PendingInclusiveDebtSummary reports any debt incl. pending plus the
	// notice kinds.
	PendingInclusiveDebtSummary(ctx context.Context, owner string) (bool, []string, error)
	// ReactedFailedTexts lists reaction-failure marker texts for owner.
	ReactedFailedTexts(ctx context.Context, owner string) ([]session.BatchReactedFailedText, error)
	// LiveWorkSummary counts live own roots and live delegation descendants
	// of owner. Counts are 1/1 (never 0/0) on an unreadable read.
	LiveWorkSummary(ctx context.Context, owner string) (own, descendants int, err error)
	// LiveScopeForOwners answers the CLI scope's running-work half per owner.
	LiveScopeForOwners(ctx context.Context, owners []string) ([]BatchLiveScopeEntry, error)
	// DelegationChildEver reports a node under parent naming child, live or
	// historical. The live read answers running delegations; the history
	// read (every root incarnation, terminal included) preserves the
	// released-child identity after the delegation completes (rule 7).
	DelegationChildEver(ctx context.Context, parent, child string) (bool, error)
	// LiveDelegationChildOf reports a live node under parent naming child
	// as its delegation child (the ON-path RunningDelegation fact).
	LiveDelegationChildOf(ctx context.Context, parent, child string) (bool, error)
	// LiveNodes lists owner's live tree nodes for the supervision summary.
	LiveNodes(ctx context.Context, owner string) ([]session.BatchLiveNode, error)
	// LiveDelegationNodes lists owner's live delegation DESCENDANT nodes
	// (the child-session facts carrying ChildSessionID) for the supervision
	// summary's delegation half. The conservative rule mirrors LiveNodes:
	// a failed or incomplete read returns what it saw plus the error.
	LiveDelegationNodes(ctx context.Context, owner string) ([]session.BatchLiveNode, error)
	// AttemptRows rereads the GIVEN acted-on snapshot after an attempt was
	// counted (per-row wake attempts against the atK threshold).
	AttemptRows(ctx context.Context, owner string, snap session.BatchDebtSnapshot, atK int) (BatchAttemptRows, error)
	// IncrementWakeAttempts counts one unreacted attempt on every row of the
	// acted-on snapshot.
	IncrementWakeAttempts(ctx context.Context, owner string, snap session.BatchDebtSnapshot) error
	// SettleReactedFailedWithMarker closes EXACTLY the snapshot's rows by
	// failure and writes the wake_failed marker; returns the settled count.
	SettleReactedFailedWithMarker(ctx context.Context, owner string, snap session.BatchDebtSnapshot, text string) (int64, error)
	// DebtToolCallIDs resolves each snapshot ref's root-row tool_call_id for
	// the settle marker text (nil when unresolvable).
	DebtToolCallIDs(ctx context.Context, snap session.BatchDebtSnapshot) []string
	// PullBatchNotices delivers selected batch completion notices into
	// history (production BatchPullNotices) and returns them with empty
	// claim ids (batch nodes carry no async claim identity).
	PullBatchNotices(ctx context.Context, messages message.Service, owner string) ([]session.PulledNotice, error)
	// PullPendingSessionNotices delivers EVERY pending session notice
	// (markers included, wake=0 rows like the wake_failed marker) WITHOUT
	// the legacy async_jobs void validation: a supervision check-in voids
	// only when the owner's batch live scope is closed; wake_only and
	// child_question rows (job_tool_call_id NULL in batch mode) deliver.
	PullPendingSessionNotices(ctx context.Context, messages message.Service, owner string, build func(session.SessionNoticeRow) message.CreateMessageParams) ([]session.PulledNotice, error)
	// HeldQuestions lists pending child-question debt rows (fenced per
	// incarnation).
	HeldQuestions(ctx context.Context, owner string) ([]session.PendingNoticeDebt, error)
	// LiveHeldQuestions lists pending child_question notices whose marker
	// names a LIVE batch node incarnation of owner (row exists, state
	// live, token matches the marker). Unparseable or stale-bound notices
	// are NOT counted; a failed read is conservative (1 + err).
	LiveHeldQuestions(ctx context.Context, owner string) ([]session.PendingNoticeDebt, error)
}

// ErrBatchFactsSourceUnavailable marks a permanently unavailable batch
// facts node source (missing binding or missing node-source dependency).
// It is a terminal condition: callers must surface it as an error, never
// retry it.
var ErrBatchFactsSourceUnavailable = errBatchFactsUnavailable

// unknownBatchCutoverFacts is the flag-ON fail-safe: no node source could be
// constructed (no binding, no live reader). It never touches the ledger and
// answers every question conservatively: debt true, live true.
type unknownBatchCutoverFacts struct{}

func (unknownBatchCutoverFacts) SelectedDebtExists(context.Context, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) CaptureVisibleDebt(context.Context, string) (session.BatchDebtSnapshot, error) {
	return session.BatchDebtSnapshot{}, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) PendingInclusiveDebt(context.Context, string) (session.BatchPendingDebt, error) {
	return session.BatchPendingDebt{}, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) PendingInclusiveDebtSummary(context.Context, string) (bool, []string, error) {
	return true, nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) ReactedFailedTexts(context.Context, string) ([]session.BatchReactedFailedText, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveWorkSummary(context.Context, string) (int, int, error) {
	return 1, 1, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveScopeForOwners(_ context.Context, owners []string) ([]BatchLiveScopeEntry, error) {
	out := make([]BatchLiveScopeEntry, 0, len(owners))
	for _, o := range owners {
		out = append(out, BatchLiveScopeEntry{Owner: o, Live: true})
	}
	return out, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveDelegationChildOf(context.Context, string, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) DelegationChildEver(context.Context, string, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) LiveNodes(context.Context, string) ([]session.BatchLiveNode, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveDelegationNodes(context.Context, string) ([]session.BatchLiveNode, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) AttemptRows(context.Context, string, session.BatchDebtSnapshot, int) (BatchAttemptRows, error) {
	return BatchAttemptRows{}, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) IncrementWakeAttempts(context.Context, string, session.BatchDebtSnapshot) error {
	return errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) SettleReactedFailedWithMarker(context.Context, string, session.BatchDebtSnapshot, string) (int64, error) {
	return 0, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) DebtToolCallIDs(context.Context, session.BatchDebtSnapshot) []string {
	return nil
}

func (unknownBatchCutoverFacts) PullBatchNotices(context.Context, message.Service, string) ([]session.PulledNotice, error) {
	return nil, nil
}

func (unknownBatchCutoverFacts) PullPendingSessionNotices(context.Context, message.Service, string, func(session.SessionNoticeRow) message.CreateMessageParams) ([]session.PulledNotice, error) {
	return nil, nil
}

func (unknownBatchCutoverFacts) HeldQuestions(context.Context, string) ([]session.PendingNoticeDebt, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveHeldQuestions(context.Context, string) ([]session.PendingNoticeDebt, error) {
	return []session.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, errBatchFactsUnavailable
}

// coordinatorBatchCutoverFactsBindings maps a *coordinator onto its
// explicitly bound fact source (R12-H wiring at install time).
var coordinatorBatchCutoverFactsBindings sync.Map

// BindCoordinatorBatchCutoverFacts binds one coordinator to the node-backed
// fact source. R12-H calls this beside BindCoordinatorBatchRuntime, e.g.
// BindCoordinatorBatchCutoverFacts(c, NewBatchCutoverNodeFacts(store, live,
// writer)). nil facts is ignored.
func BindCoordinatorBatchCutoverFacts(c Coordinator, facts BatchCutoverFacts) {
	if c == nil || facts == nil {
		return
	}
	coordinatorBatchCutoverFactsBindings.Store(c, facts)
}

// coordinatorBatchCutoverFacts returns the fact source for c: the bound node
// source, else the conservative unknown source (unwired coordinator: never
// idle, never "no debt"). There is no flag selector and no ledger adapter:
// batch node facts are the ONLY fact source.
func coordinatorBatchCutoverFacts(c *coordinator) BatchCutoverFacts {
	if c != nil {
		if v, ok := coordinatorBatchCutoverFactsBindings.Load(c); ok {
			return v.(BatchCutoverFacts)
		}
	}
	return unknownBatchCutoverFacts{}
}

// readTurnFactsBatch is readTurnFacts' only implementation: the same
// TurnFacts shape, with the DB half sourced from batch node reads and the
// in-process half (arbiter state, hint counter, external driver, gate) on
// the rehomed coordinator runtime state. Order is law: the debt rows
// first, then the memory half.
func (c *coordinator) readTurnFactsBatch(ctx context.Context, facts BatchCutoverFacts, sessionID string, f TurnFacts) (TurnFacts, error) {
	// Gate order (ASYNC-02/09): the seam fires right
	// before the arrival gate and the rows read under bgArrival, so the debt
	// rows and the over-cap set are ONE consistent pair
	// (persistBGShellCompletion holds the same gate around insert + slot
	// decision).
	if seam := readTurnFactsGateSeam.Load(); seam != nil {
		(*seam)()
	}
	if err := c.bgArrival.lock(ctx); err != nil {
		return f, fmt.Errorf("turn facts: bg arrival: %w", err)
	}
	defer c.bgArrival.unlock()
	rows, err := facts.PendingInclusiveDebt(ctx, sessionID)
	if err != nil {
		return f, fmt.Errorf("turn facts: batch debt rows: %w", err)
	}
	f.Debt.BGShellOnly = len(rows.Nodes) == 0 && len(rows.Notices) > 0
	for _, n := range rows.Notices {
		if n.Kind != session.NoticeKindBGShellDone {
			f.Debt.BGShellOnly = false
		}
	}
	f.Debt.BGShellNotices = len(rows.Notices)
	// CompletionOnly (#1270 rule 4b): notice rows count only as completions
	// for bg_shell_done and supervision kinds; batch node debt rows are
	// completion-class unconditionally (a finished node's selected notice
	// IS a completion), like job debt was on the ledger path.
	f.Debt.CompletionOnly = true
	for _, n := range rows.Notices {
		switch n.Kind {
		case session.NoticeKindBGShellDone, session.NoticeKindSupervision:
		default:
			f.Debt.CompletionOnly = false
		}
	}
	f.Debt.PendingIncl = len(rows.Nodes) > 0 || len(rows.Notices) > 0
	// The session driver marker is retained host infrastructure
	// (session_drivers, not batch rows).
	f.Session.ForeignDriverLive, err = c.foreignLiveDriver(ctx, sessionID)
	if err != nil {
		return f, fmt.Errorf("turn facts: driver marker: %w", err)
	}
	// The child's parent is the composition key: the parent's live tree scan
	// answers "a live node names this child" (= running delegation).
	parent := ""
	if c.sessions != nil {
		if sess, gerr := c.sessions.Get(ctx, sessionID); gerr == nil {
			parent = sess.ParentSessionID
		}
	}
	if parent != "" {
		f.Session.RunningDelegation, err = facts.LiveDelegationChildOf(ctx, parent, sessionID)
		if err != nil {
			return f, fmt.Errorf("turn facts: delegation state: %w", err)
		}
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
	// ---- in-process half ----
	s := c.arb.snapshot(sessionID)
	owed := make(map[int64]struct{}, len(rows.Notices))
	for _, n := range rows.Notices {
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
	f.Session.SleepAll = s.SleepAll
	f.Session.ChainLinks = s.ChainLinks
	f.Session.ChainNoticed = s.ChainNoticed
	if len(s.ChainBatchRoots) > 0 {
		f.Session.ChainBatchRoots = make([]string, 0, len(s.ChainBatchRoots))
		for root := range s.ChainBatchRoots {
			f.Session.ChainBatchRoots = append(f.Session.ChainBatchRoots, root)
		}
	}
	if s.SleepAll {
		own, descendants, liveErr := facts.LiveWorkSummary(ctx, sessionID)
		if liveErr != nil {
			return f, fmt.Errorf("turn facts: batch live work: %w", liveErr)
		}
		// Own roots plus live delegation descendants count (#1270).
		f.Session.RunningOwnJobs = own + descendants
	}
	// Rule 5: the chain guard owns the debt by IDENTITY,
	// not by shape. Batch launches carry their launched root's tool_call_id
	// (the started tool response's own identity, recorded per leg by
	// recordChainEvidence into chainBatchRoots), and every node debt row is
	// keyed by its root id -- so the ON predicate is ownership: the chain
	// defers only when EVERY pending node debt row is a completion of a
	// root the chain itself launched, and no notice row intervenes. An
	// unrelated completion (a root the chain never launched) is NOT owned,
	// so it neither defers the chain nor gets deferred by it. Rule 5 defers
	// WITHOUT pacing: there is no "waits one pace" step anywhere on this
	// path -- the deferred launch is simply refused until the ownership
	// predicate changes.
	if f.Session.ChainLinks >= turnReactionChainLimit && len(rows.Nodes) > 0 && len(rows.Notices) == 0 {
		if len(f.Session.ChainBatchRoots) > 0 {
			ids := facts.DebtToolCallIDs(ctx, session.BatchDebtSnapshot{Owner: sessionID, Nodes: rows.Nodes})
			// No resolvable identity: fail closed (no defer) rather than
			// pretend ownership over rows we cannot name.
			owns := len(ids) > 0
			for _, id := range facts.DebtToolCallIDs(ctx, session.BatchDebtSnapshot{Owner: sessionID, Nodes: rows.Nodes}) {
				found := false
				for _, root := range f.Session.ChainBatchRoots {
					if root == id {
						found = true
						break
					}
				}
				if !found {
					owns = false
					break
				}
			}
			f.Session.ChainOwnsAllDebt = owns
		}
	}
	f.Gate = s.Gate
	f.Gate.HintNow = c.driverHints.hintSeqOf(sessionID)
	f.Session.ExternallyDrivenSelf = c.driverHints.isExternalDriver(sessionID)
	return f, nil
}
