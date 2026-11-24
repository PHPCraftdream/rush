package agentcall

import (
	"context"
	"sync"

	session "github.com/PHPCraftdream/rush/internal/session"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/store/message"
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
	PullBatchNotices(ctx context.Context, messages message.Service, owner string) ([]sessionnotice.PulledNotice, error)
	// PullPendingSessionNotices delivers EVERY pending session notice
	// (markers included, wake=0 rows like the wake_failed marker) WITHOUT
	// the legacy async_jobs void validation: a supervision check-in voids
	// only when the owner's batch live scope is closed; wake_only and
	// child_question rows (job_tool_call_id NULL in batch mode) deliver.
	PullPendingSessionNotices(ctx context.Context, messages message.Service, owner string, build func(sessionnotice.SessionNoticeRow) message.CreateMessageParams) ([]sessionnotice.PulledNotice, error)
	// HeldQuestions lists pending child-question debt rows (fenced per
	// incarnation).
	HeldQuestions(ctx context.Context, owner string) ([]sessionnotice.PendingNoticeDebt, error)
	// LiveHeldQuestions lists pending child_question notices whose marker
	// names a LIVE batch node incarnation of owner (row exists, state
	// live, token matches the marker). Unparseable or stale-bound notices
	// are NOT counted; a failed read is conservative (1 + err).
	LiveHeldQuestions(ctx context.Context, owner string) ([]sessionnotice.PendingNoticeDebt, error)
}

// TurnAdmission records whether ONE agent.Run invocation executed a turn
// or was queued behind a current owner. The flag is mutex-guarded because
// the recorder is written inside Run and read only after Run has returned
// to its caller; keep it cheap, since every Run invocation through
// coordinator_run.go arms one.
type TurnAdmission struct {
	mu     sync.Mutex
	queued bool
}

// NewTurnAdmission returns a recorder whose invocation has not yet
// reported any admission outcome.
func NewTurnAdmission() *TurnAdmission { return &TurnAdmission{} }

// MarkQueued records that this invocation queued the call instead of
// executing it. Called ONLY by sessionAgent.Run's queueing branch, and
// only on the direct caller's recorder (Run detaches the recorder from
// ctx before anything downstream runs, so nested Runs -- a sub-agent's
// child session, a queue drain -- can never mark an ancestor's recorder).
func (t *TurnAdmission) MarkQueued() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.queued = true
}

// WasQueued reports whether the invocation this recorder was armed for
// queued its call instead of executing a turn.
func (t *TurnAdmission) WasQueued() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.queued
}

type turnAdmissionCtxKey struct{}

// WithTurnAdmission arms a as the admission recorder for the DIRECT Run
// invocation this ctx is handed to. Only one recorder is ever reachable
// through a ctx chain: WithoutTurnAdmission detaches it before anything
// downstream runs.
func WithTurnAdmission(ctx context.Context, a *TurnAdmission) context.Context {
	return context.WithValue(ctx, turnAdmissionCtxKey{}, a)
}

// TurnAdmissionFrom returns the recorder armed on ctx, or nil when the
// caller did not arm one (every non-coordinator Run caller).
func TurnAdmissionFrom(ctx context.Context) *TurnAdmission {
	a, _ := ctx.Value(turnAdmissionCtxKey{}).(*TurnAdmission)
	return a
}

// WithoutTurnAdmission detaches any recorder from ctx so a nested Run
// reached through this turn (sub-agent delegation, queue-drain) observes
// nil and cannot mark an ancestor invocation's admission outcome.
func WithoutTurnAdmission(ctx context.Context) context.Context {
	return context.WithValue(ctx, turnAdmissionCtxKey{}, (*TurnAdmission)(nil))
}
