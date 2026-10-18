// Node-backed BatchCutoverFacts: every answer comes from batch_nodes, the
// batch outbox and session_notices reads (plus the retained host probe).
// async_jobs is never read and terminal state is never mirrored back. Every
// failed or incomplete read answers conservatively: debt true, live true.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

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
	if n.store != nil {
		// One read transaction on the store writer: roots and their nodes
		// come from a single snapshot, index-served (the D11-profiled pull
		// path's live read).
		return n.store.BatchLiveForSessionsTx(ctx, n.live, []string{owner})
	}
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

func (n nodeBatchCutoverFacts) LiveScopeForOwners(ctx context.Context, owners []string) ([]BatchLiveScopeEntry, error) {
	var firstErr error
	if n.live == nil {
		firstErr = errBatchFactsUnavailable
	}
	var results map[string]session.BatchLiveResult
	if n.live != nil {
		if n.store != nil {
			results = n.store.BatchLiveForSessionsTx(ctx, n.live, owners)
		} else {
			results = n.live.BatchLiveForSessions(ctx, owners)
		}
	}
	out := make([]BatchLiveScopeEntry, 0, len(owners))
	for _, o := range owners {
		entry := BatchLiveScopeEntry{Owner: o, Live: true}
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
		if node.ChildSessionID == child && batchNodeStateLive(node.State) {
			return true, nil
		}
	}
	for _, node := range res.Tree {
		if node.ChildSessionID == child && batchNodeStateLive(node.State) {
			return true, nil
		}
	}
	return false, nil
}

// batchNodeStateLive mirrors the store's live-state vocabulary.
func batchNodeStateLive(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
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

func (n nodeBatchCutoverFacts) AttemptRows(ctx context.Context, owner string, snap session.BatchDebtSnapshot, atK int) (BatchAttemptRows, error) {
	if n.store == nil {
		return BatchAttemptRows{}, errBatchFactsUnavailable
	}
	// The GIVEN acted-on set only: rows another settlement closed since the
	// snapshot was taken are never re-captured into this accounting.
	out := BatchAttemptRows{AtK: session.BatchDebtSnapshot{Owner: owner}}
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
		count, err := n.store.BatchReactionMaxWakeAttempts(ctx, owner, session.BatchDebtSnapshot{Owner: owner, Notices: []session.DebtNoticeRef{ref}})
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

func (n nodeBatchCutoverFacts) HeldQuestions(ctx context.Context, owner string) ([]session.PendingNoticeDebt, error) {
	if n.store == nil {
		return []session.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, errBatchFactsUnavailable
	}
	rows, err := n.store.BatchReactionPendingInclusiveDebtRows(ctx, owner)
	if err != nil {
		// Unknown debt: a held question may exist (conservative for sleep
		// breakers), surfaced through the error.
		return []session.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, err
	}
	var out []session.PendingNoticeDebt
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
func (n nodeBatchCutoverFacts) LiveHeldQuestions(ctx context.Context, owner string) ([]session.PendingNoticeDebt, error) {
	if n.writer == nil {
		return []session.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, errBatchFactsUnavailable
	}
	rows, err := n.writer.QueryContext(ctx,
		// The retained-debt predicate (BatchOutboxDebtNotices, batch_outbox.sql):
		// pending OR already-delivered rows still hold the session while
		// unreacted and wake=1.
		`SELECT id, text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery IN ('pending','done') AND wake = 1 AND reacted = 0 ORDER BY id`,
		owner, session.NoticeKindChildQuestion)
	if err != nil {
		return []session.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, err
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
			return []session.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return []session.PendingNoticeDebt{{Kind: session.NoticeKindChildQuestion}}, err
	}
	var out []session.PendingNoticeDebt
	for _, r := range pending {
		node, token, ok := parseBatchQuestionMarker(r.text)
		if !ok {
			continue
		}
		var rowToken int64
		var state string
		err := n.writer.QueryRowContext(ctx,
			`SELECT token, state FROM batch_nodes WHERE node_id = ?1 AND owner_session_id = ?2`,
			node, owner).Scan(&rowToken, &state)
		if err != nil || !batchNodeStateLive(state) || rowToken != token {
			continue
		}
		out = append(out, session.PendingNoticeDebt{ID: r.id, Kind: session.NoticeKindChildQuestion})
	}
	return out, nil
}

// parseBatchQuestionMarker extracts (nodeID, token) from a child-question
// notice text whose header line is the production marker written by
// batchCoordinatorQuestionPublisher (batch_install_question.go):
// "[batch-question node=X token=N sequence=S depth=D child=C]". A text
// without a parseable marker (or token 0, which the publisher never writes)
// is not counted. The token parses straight into int64 -- the batch_nodes
// token column's type -- so no unbounded uint64->int64 conversion exists
// (CodeQL go/integer-conversion).
func parseBatchQuestionMarker(text string) (string, int64, bool) {
	if !strings.HasPrefix(text, "[batch-question ") {
		return "", 0, false
	}
	header := strings.TrimPrefix(text, "[batch-question ")
	if idx := strings.Index(header, "]"); idx >= 0 {
		header = header[:idx]
	}
	var node string
	var token int64
	for _, field := range strings.Fields(header) {
		name, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch name {
		case "node":
			node = value
		case "token":
			t, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return "", 0, false
			}
			token = t
		}
	}
	if node == "" || token == 0 {
		return "", 0, false
	}
	return node, token, true
}

func (n nodeBatchCutoverFacts) PullBatchNotices(ctx context.Context, messages message.Service, owner string) ([]session.PulledNotice, error) {
	if n.store == nil {
		return nil, errBatchFactsUnavailable
	}
	pulled, err := n.store.BatchPullNotices(ctx, messages, owner, buildBatchNoticeMessageParams)
	if err != nil {
		return nil, err
	}
	out := make([]session.PulledNotice, 0, len(pulled))
	for _, p := range pulled {
		// Batch nodes carry no async claim identity: the claim stays "".
		out = append(out, session.PulledNotice{Message: p.Message, Wake: p.Wake})
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
func (n nodeBatchCutoverFacts) PullPendingSessionNotices(ctx context.Context, messages message.Service, owner string, build func(session.SessionNoticeRow) message.CreateMessageParams) ([]session.PulledNotice, error) {
	if n.writer == nil {
		return nil, errBatchFactsUnavailable
	}
	// Pending rows first (idx_session_notices_owner_pending), the live-scope
	// read second and ONLY when a supervision row needs it: openScope is
	// consumed exclusively by the supervision void in pullOneSessionNotice,
	// so a pull with no pending supervision rows must not read batch_nodes
	// at all (the D11 profile: the unconditional read scanned batch_nodes on
	// every step-boundary pull as the table grew). Both reads stay outside
	// any transaction, as before.
	rows, err := n.writer.QueryContext(ctx,
		`SELECT id, kind, text, wake FROM session_notices WHERE owner = ?1 AND delivery = 'pending' ORDER BY id`,
		owner)
	if err != nil {
		return nil, err
	}
	type pendingRow struct {
		id       int64
		wake     int64
		kindText session.SessionNoticeRow
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
	var out []session.PulledNotice
	for _, r := range pending {
		msg, pulled, err := n.pullOneSessionNotice(ctx, messages, owner, r.id, r.kindText, build, openScope)
		if err != nil {
			// The row stays pending and is retried on the next pull.
			continue
		}
		if pulled {
			out = append(out, session.PulledNotice{Message: msg, Wake: r.wake == 1})
		}
	}
	return out, nil
}

// pullOneSessionNotice is one row's CAS/void/deliver transaction.
func (n nodeBatchCutoverFacts) pullOneSessionNotice(ctx context.Context, messages message.Service, owner string, id int64, row session.SessionNoticeRow, build func(session.SessionNoticeRow) message.CreateMessageParams, openScope bool) (message.Message, bool, error) {
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
