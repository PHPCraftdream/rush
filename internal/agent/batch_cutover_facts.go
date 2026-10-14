// BatchCutoverFacts is the R12-E single fact-source seam for the job-batches
// cutover: the terminal/debt/scope/reaction/wake questions the pre-cutover
// consumers direct at the async_jobs ledger. Flag OFF (RUSH_BATCH_RUNTIME
// unset/false) keeps every caller on its byte-identical ledger path; flag ON
// routes the FACT reads through the node-backed implementation, which reads
// batch_nodes/batch outbox/session_notices only and never touches async_jobs.
// Wake schedules and session driver markers are retained host infrastructure
// and stay on their existing readers on both paths.
package agent

import (
	"context"
	"errors"
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
	// PullPendingSessionNotices delivers pending wake=1 session notices
	// WITHOUT the legacy async_jobs void validation: a supervision check-in
	// voids only when the owner's batch live scope is closed; wake_only and
	// child_question rows (job_tool_call_id NULL in batch mode) deliver.
	PullPendingSessionNotices(ctx context.Context, messages message.Service, owner string, build func(session.SessionNoticeRow) message.CreateMessageParams) ([]session.PulledNotice, error)
	// HeldQuestions lists pending child-question debt rows (fenced per
	// incarnation).
	HeldQuestions(ctx context.Context, owner string) ([]session.PendingNoticeDebt, error)
}

// unknownBatchCutoverFacts is the flag-ON fail-safe: no node source could be
// constructed (no binding, no live reader). It never touches the ledger and
// answers every question conservatively: debt true, live true.
type unknownBatchCutoverFacts struct{}

func (unknownBatchCutoverFacts) SelectedDebtExists(context.Context, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) CaptureVisibleDebt(context.Context, string) (session.BatchDebtSnapshot, error) {
	return session.BatchDebtSnapshot{}, errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) PendingInclusiveDebt(context.Context, string) (session.BatchPendingDebt, error) {
	return session.BatchPendingDebt{}, errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) PendingInclusiveDebtSummary(context.Context, string) (bool, []string, error) {
	return true, nil, errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) ReactedFailedTexts(context.Context, string) ([]session.BatchReactedFailedText, error) {
	return nil, errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) LiveWorkSummary(context.Context, string) (int, int, error) {
	return 1, 1, errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) LiveScopeForOwners(_ context.Context, owners []string) ([]BatchLiveScopeEntry, error) {
	out := make([]BatchLiveScopeEntry, 0, len(owners))
	for _, o := range owners {
		out = append(out, BatchLiveScopeEntry{Owner: o, Live: true})
	}
	return out, errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) LiveDelegationChildOf(context.Context, string, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) DelegationChildEver(context.Context, string, string) (bool, error) {
	return true, nil
}

func (unknownBatchCutoverFacts) LiveNodes(context.Context, string) ([]session.BatchLiveNode, error) {
	return nil, errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) AttemptRows(context.Context, string, session.BatchDebtSnapshot, int) (BatchAttemptRows, error) {
	return BatchAttemptRows{}, errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) IncrementWakeAttempts(context.Context, string, session.BatchDebtSnapshot) error {
	return errors.New("batch facts: node source unavailable")
}

func (unknownBatchCutoverFacts) SettleReactedFailedWithMarker(context.Context, string, session.BatchDebtSnapshot, string) (int64, error) {
	return 0, errors.New("batch facts: node source unavailable")
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
	return nil, errors.New("batch facts: node source unavailable")
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

// coordinatorBatchCutoverFacts returns the flag-selected fact source for c:
// the bound node source when the batch runtime is on, else the conservative
// unknown source (flag on without wiring: never idle, never "no debt"), else
// nil -- the caller keeps its pre-cutover inline ledger path, byte-identical.
func coordinatorBatchCutoverFacts(c *coordinator) BatchCutoverFacts {
	if !batchFlagEnabled() {
		return nil
	}
	if c != nil {
		if v, ok := coordinatorBatchCutoverFactsBindings.Load(c); ok {
			return v.(BatchCutoverFacts)
		}
	}
	return unknownBatchCutoverFacts{}
}

// ledgerBatchCutoverFacts adapts the pre-cutover workLedger/store reads onto
// the seam so future call sites can branch by a single field read. Flag-OFF
// consumers keep their inline code; this adapter documents the mapping and
// is the OFF twin of the node source.
type ledgerBatchCutoverFacts struct{ c *coordinator }

// NewLedgerBatchCutoverFacts returns the ledger-backed seam over c's async
// work ledger (the pre-cutover fact source).
func NewLedgerBatchCutoverFacts(c Coordinator) BatchCutoverFacts {
	if cc, ok := c.(*coordinator); ok {
		return ledgerBatchCutoverFacts{c: cc}
	}
	return unknownBatchCutoverFacts{}
}

func (f ledgerBatchCutoverFacts) ledger() *workLedger {
	if f.c == nil {
		return nil
	}
	return f.c.asyncJobs
}

func (f ledgerBatchCutoverFacts) SelectedDebtExists(ctx context.Context, owner string) (bool, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return false, nil
	}
	return l.store.ReactionDebtExists(ctx, owner)
}

func (f ledgerBatchCutoverFacts) CaptureVisibleDebt(ctx context.Context, owner string) (session.BatchDebtSnapshot, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return session.BatchDebtSnapshot{}, nil
	}
	snap, err := l.store.CaptureDebtSnapshot(ctx, owner)
	if err != nil {
		return session.BatchDebtSnapshot{}, err
	}
	// async_jobs claim rows have no node identity: only the notice half maps.
	out := session.BatchDebtSnapshot{Owner: owner}
	out.Notices = append(out.Notices, snap.Notices...)
	return out, nil
}

func (f ledgerBatchCutoverFacts) PendingInclusiveDebt(ctx context.Context, owner string) (session.BatchPendingDebt, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return session.BatchPendingDebt{}, nil
	}
	hasJobDebt, notices, _, err := l.store.PendingInclusiveDebtRows(ctx, owner)
	if err != nil {
		return session.BatchPendingDebt{}, err
	}
	out := session.BatchPendingDebt{}
	for _, n := range notices {
		out.Notices = append(out.Notices, session.PendingNoticeDebt{ID: n.ID, Kind: n.Kind})
	}
	if hasJobDebt {
		// Job-debt presence has no node ref; the summary form keeps it.
		out.Notices = append(out.Notices, session.PendingNoticeDebt{Kind: session.NoticeKindWakeFailed})
	}
	return out, nil
}

func (f ledgerBatchCutoverFacts) PendingInclusiveDebtSummary(ctx context.Context, owner string) (bool, []string, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return false, nil, nil
	}
	return l.store.PendingInclusiveDebtSummary(ctx, owner)
}

func (f ledgerBatchCutoverFacts) ReactedFailedTexts(ctx context.Context, owner string) ([]session.BatchReactedFailedText, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return nil, nil
	}
	rows, err := l.store.ListReactedFailedText(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]session.BatchReactedFailedText, 0, len(rows))
	for _, r := range rows {
		out = append(out, session.BatchReactedFailedText{Text: r.Text})
	}
	return out, nil
}

func (f ledgerBatchCutoverFacts) LiveWorkSummary(ctx context.Context, owner string) (int, int, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return 0, 0, nil
	}
	work := l.store.LiveWorkForRoots(ctx, []string{owner})[owner]
	if work.OwnIncomplete || work.DescendantsIncomplete {
		return 1, 1, fmt.Errorf("batch facts: live work read incomplete for %s", owner)
	}
	return len(work.Own), len(work.Descendants), nil
}

func (f ledgerBatchCutoverFacts) LiveScopeForOwners(ctx context.Context, owners []string) ([]BatchLiveScopeEntry, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return nil, nil
	}
	rows, err := l.store.ListRunningForOwners(ctx, owners)
	if err != nil {
		return nil, err
	}
	live := make(map[string]bool, len(owners))
	for _, o := range owners {
		live[o] = false
	}
	for _, row := range rows {
		if l.store.HostNotDead(row.HostID) {
			live[row.Owner] = true
		}
	}
	out := make([]BatchLiveScopeEntry, 0, len(owners))
	for _, o := range owners {
		out = append(out, BatchLiveScopeEntry{Owner: o, Live: live[o]})
	}
	return out, nil
}

func (f ledgerBatchCutoverFacts) DelegationChildEver(ctx context.Context, parent, child string) (bool, error) {
	return f.LiveDelegationChildOf(ctx, parent, child)
}

func (f ledgerBatchCutoverFacts) LiveNodes(context.Context, string) ([]session.BatchLiveNode, error) {
	// The ledger adapter is OFF-path-unused and the summary is a batch read.
	return nil, nil
}

func (f ledgerBatchCutoverFacts) AttemptRows(ctx context.Context, owner string, snap session.BatchDebtSnapshot, atK int) (BatchAttemptRows, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return BatchAttemptRows{}, nil
	}
	var out BatchAttemptRows
	// Node refs have no legacy job twin: while the visible legacy snapshot
	// still holds job debt, every snapshot node ref is unmappable at K.
	visible, err := l.store.CaptureDebtSnapshot(ctx, owner)
	if err != nil {
		return out, err
	}
	if len(visible.Jobs) > 0 {
		out.UnmappedAtK += len(snap.Nodes)
	}
	for _, ref := range snap.Notices {
		n, err := l.store.MaxWakeAttempts(ctx, owner, session.DebtSnapshot{Notices: []session.DebtNoticeRef{ref}})
		if err != nil {
			return out, err
		}
		if n == 0 {
			continue
		}
		out.Open++
		out.Max = max(out.Max, n)
		if n >= atK {
			out.AtK.Notices = append(out.AtK.Notices, ref)
		}
	}
	return out, nil
}

func (f ledgerBatchCutoverFacts) IncrementWakeAttempts(ctx context.Context, owner string, snap session.BatchDebtSnapshot) error {
	l := f.ledger()
	if l == nil || l.store == nil {
		return nil
	}
	// Node refs have no legacy twin: only the notice half maps.
	return l.store.IncrementWakeAttempts(ctx, owner, session.DebtSnapshot{Notices: snap.Notices})
}

func (f ledgerBatchCutoverFacts) SettleReactedFailedWithMarker(ctx context.Context, owner string, snap session.BatchDebtSnapshot, text string) (int64, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return 0, nil
	}
	// Node refs have no legacy twin: only the notice half maps.
	return l.store.SettleReactedFailedWithMarker(ctx, owner, session.DebtSnapshot{Notices: snap.Notices}, text)
}

func (f ledgerBatchCutoverFacts) DebtToolCallIDs(context.Context, session.BatchDebtSnapshot) []string {
	// Legacy job refs carry their own ToolCallID; the adapter is unused on
	// the ON path and returns nothing here.
	return nil
}

func (f ledgerBatchCutoverFacts) PullBatchNotices(context.Context, message.Service, string) ([]session.PulledNotice, error) {
	return nil, nil
}

func (f ledgerBatchCutoverFacts) PullPendingSessionNotices(ctx context.Context, messages message.Service, owner string, build func(session.SessionNoticeRow) message.CreateMessageParams) ([]session.PulledNotice, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return nil, nil
	}
	return l.store.PullSessionNotices(ctx, messages, owner, build)
}

func (f ledgerBatchCutoverFacts) LiveDelegationChildOf(ctx context.Context, parent, child string) (bool, error) {
	l := f.ledger()
	if l == nil || l.store == nil {
		return false, nil
	}
	jobs, err := l.store.ListAsyncJobsForOwner(ctx, parent)
	if err != nil {
		return false, err
	}
	for _, job := range jobs {
		if job.ChildSessionID.Valid && job.ChildSessionID.String == child {
			return true, nil
		}
	}
	return false, nil
}

func (f ledgerBatchCutoverFacts) HeldQuestions(ctx context.Context, owner string) ([]session.PendingNoticeDebt, error) {
	rows, err := f.PendingInclusiveDebt(ctx, owner)
	if err != nil {
		return nil, err
	}
	var out []session.PendingNoticeDebt
	for _, n := range rows.Notices {
		if n.Kind == session.NoticeKindChildQuestion {
			out = append(out, n)
		}
	}
	return out, nil
}

// readTurnFactsBatch is readTurnFacts' flag-on twin: the same TurnFacts
// shape, with the DB half sourced from batch node reads instead of the
// async_jobs ledger. The in-process half (arbiter state, hint counter,
// external driver, gate) is identical state on both paths. Order is law, as
// in readTurnFacts: the debt rows first, then the memory half.
func (c *coordinator) readTurnFactsBatch(ctx context.Context, facts BatchCutoverFacts, sessionID string, f TurnFacts, l *workLedger) (TurnFacts, error) {
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
	// The session driver marker is retained host infrastructure: read on
	// both paths (session_drivers, not async_jobs).
	f.Session.ForeignDriverLive, err = l.foreignLiveDriver(ctx, sessionID)
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
	// ---- in-process half (identical state to the ledger path) ----
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
	if s.SleepAll {
		own, descendants, liveErr := facts.LiveWorkSummary(ctx, sessionID)
		if liveErr != nil {
			return f, fmt.Errorf("turn facts: batch live work: %w", liveErr)
		}
		// Own roots plus live delegation descendants count, exactly like
		// LiveWorkForRoots on the ledger path (#1270).
		f.Session.RunningOwnJobs = own + descendants
	}
	// Rule 5 on the flag-on path: the chain's idle launches are batch node
	// completions (their claims were async identities), so the guard owns
	// the debt when every pending row is a node completion and no notice
	// row (supervision, question, marker) is mixed in.
	if f.Session.ChainLinks >= turnReactionChainLimit && len(rows.Nodes) > 0 && len(rows.Notices) == 0 {
		f.Session.ChainOwnsAllDebt = true
	}
	f.Gate = s.Gate
	f.Gate.HintNow = l.hintSeqOf(sessionID)
	f.Session.ExternallyDrivenSelf = l.isExternalDriver(sessionID)
	return f, nil
}
