package agent

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/session"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// BatchCutoverFacts is the R12-E single fact-source seam for the job-batches
// cutover: the terminal/debt/scope/reaction/wake questions the pre-cutover
// consumers direct at the async_jobs ledger. The node-backed implementation
// is the only source: it reads batch_nodes/batch outbox/session_notices only
// and never touches async_jobs (the legacy adapter is gone). Wake schedules
// and session driver markers are retained host infrastructure and stay on
// their existing readers.
// BatchCutoverFacts is retained as a root-package alias for its anchored
// coordinator, supervision, drain, adapter, and test callers.
type BatchCutoverFacts = agentcall.BatchCutoverFacts

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

func (unknownBatchCutoverFacts) LiveScopeForOwners(_ context.Context, owners []string) ([]agentcall.BatchLiveScopeEntry, error) {
	out := make([]agentcall.BatchLiveScopeEntry, 0, len(owners))
	for _, o := range owners {
		out = append(out, agentcall.BatchLiveScopeEntry{Owner: o, Live: true})
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

func (unknownBatchCutoverFacts) AttemptRows(context.Context, string, session.BatchDebtSnapshot, int) (agentcall.BatchAttemptRows, error) {
	return agentcall.BatchAttemptRows{}, errBatchFactsUnavailable
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

func (unknownBatchCutoverFacts) PullBatchNotices(context.Context, message.Service, string) ([]sessionnotice.PulledNotice, error) {
	return nil, nil
}

func (unknownBatchCutoverFacts) PullPendingSessionNotices(context.Context, message.Service, string, func(sessionnotice.SessionNoticeRow) message.CreateMessageParams) ([]sessionnotice.PulledNotice, error) {
	return nil, nil
}

func (unknownBatchCutoverFacts) HeldQuestions(context.Context, string) ([]sessionnotice.PendingNoticeDebt, error) {
	return nil, errBatchFactsUnavailable
}

func (unknownBatchCutoverFacts) LiveHeldQuestions(context.Context, string) ([]sessionnotice.PendingNoticeDebt, error) {
	return []sessionnotice.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, errBatchFactsUnavailable
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
func (c *coordinator) BatchFactsBindingKey() any { return c }

func coordinatorBatchCutoverFacts(c agentcall.CoordinatorBatchFacts) BatchCutoverFacts {
	if c != nil {
		if v, ok := coordinatorBatchCutoverFactsBindings.Load(c.BatchFactsBindingKey()); ok {
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
	return newBatchCutoverGlueComponent(c).readTurnFactsBatch(ctx, facts, sessionID, f)
}

// Node-backed BatchCutoverFacts: every answer comes from batch_nodes, the
// batch outbox and session_notices reads (plus the retained host probe).
// async_jobs is never read and terminal state is never mirrored back. Every
// failed or incomplete read answers conservatively: debt true, live true.
// nodeBatchCutoverFacts is constructed by the installer over one BatchStore,
// its live reader and the same writer conn the store wraps
// (NewBatchCutoverNodeFacts) and bound to a coordinator with
// BindCoordinatorBatchCutoverFacts.
type nodeBatchCutoverFacts struct {
	store  *session.BatchStore
	live   *session.BatchLiveReader
	writer *sql.DB
}

// NewBatchCutoverNodeFacts builds the flag-ON fact source over one batch
// store, its live reader and the store's writer conn (the same *sql.DB
// session.NewBatchStore wraps; R12-H wiring passes it as the third
// argument). A nil live reader degrades liveness answers to conservative
// "live" (never idle); a nil store fails every debt answer; a nil writer
// only degrades DebtToolCallIDs (the settle marker falls back to the
// provider-error wording).
func NewBatchCutoverNodeFacts(store *session.BatchStore, live *session.BatchLiveReader, writer *sql.DB) BatchCutoverFacts {
	return nodeBatchCutoverFacts{store: store, live: live, writer: writer}
}

func (n nodeBatchCutoverFacts) SelectedDebtExists(ctx context.Context, owner string) (bool, error) {
	if n.store == nil {
		return true, errBatchFactsUnavailable
	}
	debt, err := n.store.BatchReactionDebtExists(ctx, owner)
	if err != nil {
		// Unknown debt must not read as "no debt".
		return true, err
	}
	return debt, nil
}

func (n nodeBatchCutoverFacts) CaptureVisibleDebt(ctx context.Context, owner string) (session.BatchDebtSnapshot, error) {
	if n.store == nil {
		return session.BatchDebtSnapshot{Owner: owner}, errBatchFactsUnavailable
	}
	snap, err := n.store.BatchReactionCaptureDebtSnapshot(ctx, owner)
	if err != nil {
		return session.BatchDebtSnapshot{Owner: owner}, err
	}
	return snap, nil
}

func (n nodeBatchCutoverFacts) PendingInclusiveDebt(ctx context.Context, owner string) (session.BatchPendingDebt, error) {
	if n.store == nil {
		return session.BatchPendingDebt{}, errBatchFactsUnavailable
	}
	rows, err := n.store.BatchReactionPendingInclusiveDebtRows(ctx, owner)
	if err != nil {
		return session.BatchPendingDebt{}, err
	}
	return rows, nil
}

func (n nodeBatchCutoverFacts) PendingInclusiveDebtSummary(ctx context.Context, owner string) (bool, []string, error) {
	if n.store == nil {
		return true, nil, errBatchFactsUnavailable
	}
	hasDebt, kinds, err := n.store.BatchReactionPendingInclusiveDebtSummary(ctx, owner)
	if err != nil {
		// Unknown debt stays pending (never "no debt").
		return true, nil, err
	}
	return hasDebt, kinds, nil
}

func (n nodeBatchCutoverFacts) ReactedFailedTexts(ctx context.Context, owner string) ([]session.BatchReactedFailedText, error) {
	if n.store == nil {
		return nil, nil
	}
	return n.store.BatchReactionListReactedFailedText(ctx, owner)
}

func (n nodeBatchCutoverFacts) liveResult(ctx context.Context, owner string) session.BatchLiveResult {
	if n.live == nil {
		// No reader: conservatively live.
		return session.BatchLiveResult{}
	}
	// The reader's TxStarter (set by the app install) routes this through
	// ONE read-only transaction on the writer: single snapshot, index-served.
	return n.live.BatchLiveForOwner(ctx, owner)
}

func (n nodeBatchCutoverFacts) LiveWorkSummary(ctx context.Context, owner string) (int, int, error) {
	res := n.liveResult(ctx, owner)
	if !res.Complete || res.Err != nil {
		// Incomplete read: conservatively live on both halves.
		return 1, 1, res.Err
	}
	return len(res.Own), len(res.Descendants), nil
}

func (n nodeBatchCutoverFacts) LiveScopeForOwners(ctx context.Context, owners []string) ([]agentcall.BatchLiveScopeEntry, error) {
	var firstErr error
	if n.live == nil {
		firstErr = errBatchFactsUnavailable
	}
	var results map[string]session.BatchLiveResult
	if n.live != nil {
		results = n.live.BatchLiveForSessions(ctx, owners)
	}
	out := make([]agentcall.BatchLiveScopeEntry, 0, len(owners))
	for _, o := range owners {
		entry := agentcall.BatchLiveScopeEntry{Owner: o, Live: true}
		if res, ok := results[o]; ok && res.Complete && res.Err == nil {
			entry.Live = res.HasLive()
		}
		out = append(out, entry)
	}
	return out, firstErr
}

func (n nodeBatchCutoverFacts) LiveDelegationChildOf(ctx context.Context, parent, child string) (bool, error) {
	res := n.liveResult(ctx, parent)
	if !res.Complete || res.Err != nil {
		// Unknown identity keeps the child delegated (drainPolicy fails closed).
		return true, res.Err
	}
	// Tree carries the parent's root facts; Descendants carries every node
	// naming a delegation child (child_session_id lives on child nodes,
	// never on roots). The live scan includes terminal children of live
	// roots, so only a LIVE-state delegation counts as running.
	for _, node := range res.Descendants {
		if node.ChildSessionID == child && batchmeta.BatchNodeStateLive(node.State) {
			return true, nil
		}
	}
	for _, node := range res.Tree {
		if node.ChildSessionID == child && batchmeta.BatchNodeStateLive(node.State) {
			return true, nil
		}
	}
	return false, nil
}

// DelegationChildEver reports a node under parent naming child, live or
// historical. The live read answers running delegations; the historical
// read (any batch node row of the parent naming child, terminal rows
// included) preserves the released-child identity after the delegation
// completes (rule 7). Root-incarnation views are NOT sufficient here: root
// rows carry no child_session_id, only their descendants do. An unreadable
// read fails closed as delegated (true).
func (n nodeBatchCutoverFacts) DelegationChildEver(ctx context.Context, parent, child string) (bool, error) {
	live, err := n.LiveDelegationChildOf(ctx, parent, child)
	if live || err != nil {
		return true, err
	}
	if n.writer == nil {
		return false, errBatchFactsUnavailable
	}
	var one int
	herr := n.writer.QueryRowContext(ctx,
		`SELECT 1 FROM batch_nodes WHERE owner_session_id = ?1 AND child_session_id = ?2 LIMIT 1`,
		parent, child).Scan(&one)
	if errors.Is(herr, sql.ErrNoRows) {
		return false, nil
	}
	if herr != nil {
		return true, herr
	}
	return true, nil
}

func (n nodeBatchCutoverFacts) LiveNodes(ctx context.Context, owner string) ([]session.BatchLiveNode, error) {
	res := n.liveResult(ctx, owner)
	if !res.Complete || res.Err != nil {
		// Conservative non-empty: the summary must not claim a quiet scope.
		return res.Tree, res.Err
	}
	return res.Tree, nil
}

// LiveDelegationNodes lists owner's live delegation descendants (the
// child-session facts carrying ChildSessionID, batch_read_live.go's
// Descendants) for the supervision summary's delegation half. The
// conservative rule mirrors LiveNodes: an incomplete or failed read returns
// what it saw plus the error.
func (n nodeBatchCutoverFacts) LiveDelegationNodes(ctx context.Context, owner string) ([]session.BatchLiveNode, error) {
	res := n.liveResult(ctx, owner)
	if !res.Complete || res.Err != nil {
		return res.Descendants, res.Err
	}
	return res.Descendants, nil
}

// errBatchFactsUnavailable marks a missing node source dependency.
var errBatchFactsUnavailable = errors.New("batch facts: node source unavailable")

func (n nodeBatchCutoverFacts) AttemptRows(ctx context.Context, owner string, snap session.BatchDebtSnapshot, atK int) (agentcall.BatchAttemptRows, error) {
	if n.store == nil {
		return agentcall.BatchAttemptRows{}, errBatchFactsUnavailable
	}
	// The GIVEN acted-on set only: rows another settlement closed since the
	// snapshot was taken are never re-captured into this accounting.
	out := agentcall.BatchAttemptRows{AtK: session.BatchDebtSnapshot{Owner: owner}}
	for _, ref := range snap.Nodes {
		count, err := n.store.BatchReactionMaxWakeAttempts(ctx, owner, session.BatchDebtSnapshot{Owner: owner, Nodes: []session.BatchDebtNodeRef{ref}})
		if err != nil {
			return out, err
		}
		if count == 0 {
			continue
		}
		out.Open++
		out.Max = max(out.Max, count)
		if count >= atK {
			out.AtK.Nodes = append(out.AtK.Nodes, ref)
		}
	}
	for _, ref := range snap.Notices {
		count, err := n.store.BatchReactionMaxWakeAttempts(ctx, owner, session.BatchDebtSnapshot{Owner: owner, Notices: []sessionnotice.DebtNoticeRef{ref}})
		if err != nil {
			return out, err
		}
		if count == 0 {
			continue
		}
		out.Open++
		out.Max = max(out.Max, count)
		if count >= atK {
			out.AtK.Notices = append(out.AtK.Notices, ref)
		}
	}
	return out, nil
}

func (n nodeBatchCutoverFacts) IncrementWakeAttempts(ctx context.Context, owner string, snap session.BatchDebtSnapshot) error {
	if n.store == nil {
		return errBatchFactsUnavailable
	}
	if snap.Empty() {
		return nil
	}
	return n.store.BatchReactionIncrementWakeAttempts(ctx, owner, snap)
}

func (n nodeBatchCutoverFacts) SettleReactedFailedWithMarker(ctx context.Context, owner string, snap session.BatchDebtSnapshot, text string) (int64, error) {
	if n.store == nil {
		return 0, errBatchFactsUnavailable
	}
	if snap.Empty() {
		return 0, nil
	}
	return n.store.BatchReactionSettleReactedFailedWithMarker(ctx, owner, snap, text)
}

// DebtToolCallIDs resolves each snapshot ref's root-row tool_call_id for the
// settle marker (deduped per RootID, NULLs skipped). A nil writer or a
// failed read returns nil: the marker falls back to the provider-error
// wording.
func (n nodeBatchCutoverFacts) DebtToolCallIDs(ctx context.Context, snap session.BatchDebtSnapshot) []string {
	if n.writer == nil {
		return nil
	}
	seen := make(map[string]bool, len(snap.Nodes))
	var ids []string
	for _, ref := range snap.Nodes {
		if seen[ref.RootID] {
			continue
		}
		seen[ref.RootID] = true
		var call sql.NullString
		if err := n.writer.QueryRowContext(ctx,
			`SELECT tool_call_id FROM batch_nodes WHERE root_id = ?1 AND node_id = root_id`,
			ref.RootID).Scan(&call); err != nil {
			return nil
		}
		if call.Valid && call.String != "" {
			ids = append(ids, call.String)
		}
	}
	return ids
}

func (n nodeBatchCutoverFacts) HeldQuestions(ctx context.Context, owner string) ([]sessionnotice.PendingNoticeDebt, error) {
	if n.store == nil {
		return []sessionnotice.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, errBatchFactsUnavailable
	}
	rows, err := n.store.BatchReactionPendingInclusiveDebtRows(ctx, owner)
	if err != nil {
		// Unknown debt: a held question may exist (conservative for sleep
		// breakers), surfaced through the error.
		return []sessionnotice.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, err
	}
	var out []sessionnotice.PendingNoticeDebt
	for _, notice := range rows.Notices {
		if notice.Kind == session.NoticeKindChildQuestion {
			out = append(out, notice)
		}
	}
	return out, nil
}

// LiveHeldQuestions lists pending-or-delivered unreacted child_question
// notices whose
// marker names a LIVE batch node incarnation of owner: the notice text must
// carry the production marker header, and the named batch_nodes row must
// exist for this owner with a live state and the marker's exact token
// (incarnation fence). Notices without a parseable marker are NOT counted.
func (n nodeBatchCutoverFacts) LiveHeldQuestions(ctx context.Context, owner string) ([]sessionnotice.PendingNoticeDebt, error) {
	if n.writer == nil {
		return []sessionnotice.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, errBatchFactsUnavailable
	}
	rows, err := n.writer.QueryContext(ctx,
		// The retained-debt predicate (BatchOutboxDebtNotices, batch_outbox.sql):
		// pending OR already-delivered rows still hold the session while
		// unreacted and wake=1.
		`SELECT id, text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery IN ('pending','done') AND wake = 1 AND reacted = 0 ORDER BY id`,
		owner, session.NoticeKindChildQuestion)
	if err != nil {
		return []sessionnotice.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, err
	}
	defer rows.Close()
	type heldRow struct {
		id   int64
		text string
	}
	var pending []heldRow
	for rows.Next() {
		var r heldRow
		if err := rows.Scan(&r.id, &r.text); err != nil {
			return []sessionnotice.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return []sessionnotice.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, err
	}
	var out []sessionnotice.PendingNoticeDebt
	for _, r := range pending {
		node, token, ok := batchmeta.ParseBatchQuestionMarker(r.text)
		if !ok {
			continue
		}
		var rowToken int64
		var state string
		err := n.writer.QueryRowContext(ctx,
			`SELECT token, state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			node, owner).Scan(&rowToken, &state)
		if err != nil || !batchmeta.BatchNodeStateLive(state) || rowToken != token {
			continue
		}
		out = append(out, sessionnotice.PendingNoticeDebt{ID: r.id, Kind: session.NoticeKindChildQuestion, NodeID: node, ChildSessionID: batchmeta.BatchQuestionMarkerChild(r.text)})
	}
	return out, nil
}

// ActionableHeldQuestions lists the owner's held questions that still await
// an ACCEPTED answer, independent of `reacted` (the owner's reaction flow
// marks every notice reacted without answering the child). The sink lookup
// and the await refusal share this fence. A nil writer is conservative:
// one unknown route plus errBatchFactsUnavailable, mirroring
// LiveHeldQuestions.
func (n nodeBatchCutoverFacts) ActionableHeldQuestions(ctx context.Context, owner string) ([]batchlife.BatchHeldQuestionRoute, error) {
	return batchActionableHeldQuestionRoutes(ctx, n.writer, owner)
}

func (n nodeBatchCutoverFacts) PullBatchNotices(ctx context.Context, messages message.Service, owner string) ([]sessionnotice.PulledNotice, error) {
	if n.store == nil {
		return nil, errBatchFactsUnavailable
	}
	pulled, err := n.store.BatchPullNotices(ctx, messages, owner, drainctl.BuildBatchNoticeMessageParams)
	if err != nil {
		return nil, err
	}
	out := make([]sessionnotice.PulledNotice, 0, len(pulled))
	for _, p := range pulled {
		// Batch nodes carry no async claim identity: the claim stays "".
		out = append(out, sessionnotice.PulledNotice{Message: p.Message, Wake: p.Wake})
	}
	return out, nil
}

// PullPendingSessionNotices delivers EVERY pending session notice (markers
// included, wake=0 rows like the wake_failed and reaction-chain markers),
// without the legacy async_jobs void validation. A supervision check-in
// voids only when the owner's batch live scope is closed (conservative: an
// unreadable liveness read keeps the check-in); wake_only and
// child_question rows (job_tool_call_id NULL in batch mode) deliver. Tx
// shape mirrors AsyncJobStore.pullOneSessionNotice. The live-scope read runs
// only when a pending supervision row is present; a pull with nothing
// pending never reads batch_nodes.
func (n nodeBatchCutoverFacts) PullPendingSessionNotices(ctx context.Context, messages message.Service, owner string, build func(sessionnotice.SessionNoticeRow) message.CreateMessageParams) ([]sessionnotice.PulledNotice, error) {
	if n.writer == nil {
		return nil, errBatchFactsUnavailable
	}
	// Pending rows first (idx_session_notices_owner_pending), the live-scope
	// read second and ONLY when a supervision row needs it: openScope is
	// consumed exclusively by the supervision void in pullOneSessionNotice,
	// so a pull with no pending supervision rows must not read batch_nodes
	// at all (the D11 profile: the unconditional read scanned batch_nodes on
	// every step-boundary pull as the table grew). The live-scope read runs
	// inside ONE read-only transaction on the reader's TxStarter
	// (BatchLiveForSessions' own dispatch); a reader built without a
	// TxStarter falls back to the single-statement join.
	rows, err := n.writer.QueryContext(ctx,
		`SELECT id, kind, text, wake FROM session_notices WHERE owner = ?1 AND delivery = 'pending' ORDER BY id`,
		owner)
	if err != nil {
		return nil, err
	}
	type pendingRow struct {
		id       int64
		wake     int64
		kindText sessionnotice.SessionNoticeRow
	}
	var pending []pendingRow
	defer rows.Close()
	for rows.Next() {
		var r pendingRow
		if err := rows.Scan(&r.id, &r.kindText.Kind, &r.kindText.Text, &r.wake); err != nil {
			return nil, err
		}
		r.kindText.ID = r.id
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The deferred Close's error is intentionally not surfaced: scan/Err
	// errors above already return (the logDebtRows pattern).
	// Lazy live-scope read: only a pending supervision row consumes
	// openScope (the check-in void in pullOneSessionNotice).
	openScope := false
	for _, r := range pending {
		if r.kindText.Kind == session.NoticeKindSupervision {
			live, descendants, lerr := n.LiveWorkSummary(ctx, owner)
			openScope = lerr != nil || live > 0 || descendants > 0
			break
		}
	}
	var out []sessionnotice.PulledNotice
	for _, r := range pending {
		msg, pulled, err := n.pullOneSessionNotice(ctx, messages, owner, r.id, r.kindText, build, openScope)
		if err != nil {
			// The row stays pending and is retried on the next pull.
			continue
		}
		if pulled {
			out = append(out, sessionnotice.PulledNotice{Message: msg, Wake: r.wake == 1})
		}
	}
	return out, nil
}

// pullOneSessionNotice is one row's CAS/void/deliver transaction.
func (n nodeBatchCutoverFacts) pullOneSessionNotice(ctx context.Context, messages message.Service, owner string, id int64, row sessionnotice.SessionNoticeRow, build func(sessionnotice.SessionNoticeRow) message.CreateMessageParams, openScope bool) (message.Message, bool, error) {
	tx, err := n.writer.BeginTx(ctx, nil)
	if err != nil {
		return message.Message{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx,
		`UPDATE session_notices SET delivery = 'done', updated_at = ? WHERE id = ? AND delivery = 'pending'`,
		now, id)
	if err != nil {
		return message.Message{}, false, err
	}
	if affected, aerr := res.RowsAffected(); aerr != nil || affected == 0 {
		// Another leader won this row.
		return message.Message{}, false, aerr
	}
	if row.Kind == session.NoticeKindSupervision && !openScope {
		if _, err := tx.ExecContext(ctx,
			`UPDATE session_notices SET delivery = 'void', updated_at = ? WHERE id = ?`,
			now, id); err != nil {
			return message.Message{}, false, err
		}
		if err := tx.Commit(); err != nil {
			return message.Message{}, false, err
		}
		return message.Message{}, false, nil
	}
	msg, err := messages.CreateTx(ctx, tx, owner, build(row))
	if err != nil {
		return message.Message{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE session_notices SET notice_message_id = ?, updated_at = ? WHERE id = ?`,
		sql.NullString{String: msg.ID, Valid: true}, now, id); err != nil {
		return message.Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return message.Message{}, false, err
	}
	// Publish AFTER commit, exactly like message.Service's own path.
	messages.PublishCreated(msg)
	return msg, true, nil
}

func batchNodeStateLive(state string) bool { return batchmeta.BatchNodeStateLive(state) }

func parseBatchQuestionMarker(text string) (string, int64, bool) {
	return batchmeta.ParseBatchQuestionMarker(text)
}

func batchQuestionMarkerChild(text string) string { return batchmeta.BatchQuestionMarkerChild(text) }

// Exact-incarnation rerun stop routing (R12-F, cutover acceptance 3): the
// batch-only post-commit executor stop (the retained rerun contract).
// The durable reconciliation is session.BatchStore.BatchRerunReconcile's
// single transaction (R10c); retirement is DURABLE and written THERE, in the
// truncation transaction (R12-F round 4: the live-root void runs in-tx, so
// it survives restarts and needs no in-memory registry). This goroutine runs
// only AFTER that transaction committed, from the Voided set it returned,
// and only stops executors: it never writes delivery state. Every entry
// carries its exact root_id, so stops are keyed by (owner, root_id) — never
// widened to a tool_call_id: a provider-reused call id can never cancel a
// NEW incarnation, and a late completion of a stopped old incarnation can
// only commit against its own (void-delivery or retired) root rows, never
// into the new one (I5).
// Rerun stop routing is batch-only: the post-commit executor stop is
// Coordinator.BatchRerunStopVoided (batch_cutover_rerun.go), the retained
// implementation of the optional BatchRerunStopper surface. The legacy
// StopRerunJobs / []session.VoidedAsyncJob contract is gone — the durable
// reconciliation (session.BatchStore.BatchRerunReconcile) returns the
// exact-incarnation Voided node set the batch stop consumes.
// BatchRerunStopper is the rerun stop surface (the only one; the legacy
// StopRerunJobs / session.VoidedAsyncJob contract is gone). It is an
// OPTIONAL interface so coordinator fakes can stay narrow: the rerun
// handler type-asserts it.
type BatchRerunStopper interface {
	BatchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode)
}

// batchRerunStopWorthy reports whether a voided incarnation's executor can
// still be stopped: the live states. A terminal incarnation is already
// settled; a stop would only chase dead rows.
func batchRerunStopWorthy(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// batchRerunRuntime unwraps the coordinator's bound batch runtime into the
// engine handle, owning host and batch store the stop needs. The binding is
// per-coordinator (BindCoordinatorBatchRuntime, batch_install_ingress.go);
// nil means not bound — the caller and its log treat that as "retire by
// delivery fencing only".
func batchRerunRuntime(c *coordinator) (runner batchcore.RunnerHandle, host string, store *session.BatchStore) {
	bind := coordinatorBatchBindingFor(c)
	if bind == nil {
		return nil, "", nil
	}
	wi, ok := bind.ingress.(*batchWorkIngress)
	if !ok || wi == nil || wi.rt == nil {
		return nil, "", nil
	}
	if attach, ok := wi.rt.cfg.Attach.(batchAttachAdapter); ok {
		store = attach.store
	}
	return wi.rt.cfg.Runner, wi.rt.cfg.Host, store
}

// BatchRerunStopVoided implements the post-commit stop: stop exactly
// the local live engine roots the committed reconciliation voided, and stop
// every voided delegation's child tree (the retained transitive-stop shape). The durable void marks were already written by
// BatchRerunReconcile in its truncation transaction; nothing here touches
// delivery state.
// Best effort and idempotent: the rows are already void-delivery, and the
// re-read below is the repeated-stop suppression — after the first pass the
// stopped root settles terminal, so a second pass over the same Voided set
// is a no-op. A stop that misses (foreign host, engine gone, already
// retired) only means the executor's completion commits void instead of
// being stopped early.
func (c *coordinator) BatchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode) {
	newBatchCutoverGlueComponent(c).batchRerunStopVoided(ctx, sessionID, voided)
}
