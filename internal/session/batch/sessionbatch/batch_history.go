package sessionbatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	sessionbatchread "github.com/PHPCraftdream/rush/internal/session/batch/sessionbatchread"
	session "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/google/uuid"
)

// Read-only activity snapshots (R10c chunk 3, inert): one structured snapshot
// per root incarnation for the sessions jobs/why/show/list/audit readers.
// Facts, counts and the host verdict come from the R10a BatchLiveReader (its
// injectable host probe is reused, never bypassed); delivery/debt columns are
// joined from the same BatchLevelNodes rows. Structured, not text-parsed; no
// duplication of the R9/R10a readers.

// BatchHistorySnapshot is the read-only state of one root incarnation:
// R10a live facts per node plus delivery/debt columns, the task counts, the
// host verdict for the root's host and the outstanding debt total (nodes
// with wake = 1, reacted = 0, delivery != 'void'). Disappeared mirrors the
// R10a level result: the root was deleted between capture and read. Complete
// mirrors level.Complete: an incomplete walk must never read as a finished
// or empty verdict (conservative-read rule).
type BatchHistorySnapshot struct {
	Owner, RootID string
	Root          BatchHistoryNodeView
	Nodes         []BatchHistoryNodeView
	Counts        sessionbatchread.BatchLiveCounts
	Debt          int64
	Host          sessionhosts.HostLockStatus
	Disappeared   bool
	Complete      bool
}

func batchHistoryNodeView(fact sessionbatchread.BatchLiveNode, n db.BatchNode) BatchHistoryNodeView {
	return BatchHistoryNodeView{
		Fact: fact, Delivery: n.Delivery, Wake: n.Wake != 0,
		Reacted: n.Reacted != 0, MessageID: n.NoticeMessageID.String,
	}
}

func batchHistoryDebtRow(n db.BatchNode) bool {
	return n.NoticeSelected == 1 && n.Wake == 1 && n.Reacted == 0 && n.Delivery != "void"
}

// BatchHistorySnapshot reads one root incarnation. The reader supplies the
// source and the host probe; a nil reader or source is an error, not an
// empty snapshot (a zero value cannot prove absence).
func (s *BatchStore) BatchHistorySnapshot(ctx context.Context, reader *sessionbatchread.BatchLiveReader, owner, root string) (BatchHistorySnapshot, error) {
	if owner == "" || root == "" {
		return BatchHistorySnapshot{}, errors.New("batch snapshot: owner and root are required")
	}
	if reader == nil || reader.Source == nil {
		return BatchHistorySnapshot{}, sessionbatchread.ErrBatchReadUnavailable
	}
	rows, err := reader.Source.BatchLevelNodes(ctx, db.BatchLevelNodesParams{Owner: owner, RootID: root})
	if err != nil {
		return BatchHistorySnapshot{}, fmt.Errorf("batch snapshot: level nodes: %w", err)
	}
	level := reader.BatchLevelRows(sessionbatchread.BatchLiveRef{OwnerSessionID: owner, RootID: root}, "", rows)
	out := BatchHistorySnapshot{Owner: owner, RootID: root, Disappeared: level.Disappeared, Complete: level.Complete}
	byID := make(map[string]db.BatchNode, len(rows))
	for _, n := range rows {
		if n.OwnerSessionID == owner && n.RootID == root {
			byID[n.NodeID] = n
		}
	}
	// Pre-order from the level walk keeps parents before children and
	// siblings in ordinal order; rows the walk missed (cycle guard) keep
	// their raw facts out of the snapshot.
	for _, fact := range level.Subtree {
		n, ok := byID[fact.NodeID]
		if !ok {
			continue
		}
		view := batchHistoryNodeView(fact, n)
		out.Nodes = append(out.Nodes, view)
		out.Counts.Add(n)
		if batchHistoryDebtRow(n) {
			out.Debt++
		}
		if n.NodeID == n.RootID {
			out.Root = view
		}
	}
	out.Host = level.Node.HostStatus
	return out, nil
}

// BatchHistoryRootViews is the oldest-first list of an owner's root
// incarnations, terminal included, each with its delivery/debt state and the
// reader's host verdict (sessions jobs/list input; D-R10c open decision:
// presentation mixing with legacy rows is a CLI decision at R12).
func (s *BatchStore) BatchHistoryRootViews(ctx context.Context, reader *sessionbatchread.BatchLiveReader, owner string) ([]BatchHistoryNodeView, error) {
	if owner == "" {
		return nil, errors.New("batch roots: owner is required")
	}
	if reader == nil || reader.Source == nil {
		return nil, sessionbatchread.ErrBatchReadUnavailable
	}
	roots, err := db.New(s.writer).BatchHistoryRootsForOwner(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("batch roots: list: %w", err)
	}
	out := make([]BatchHistoryNodeView, 0, len(roots))
	for _, r := range roots {
		out = append(out, BatchHistoryNodeView{
			Fact:     sessionbatchread.BatchLiveFact(r, r.ToolCallID.String, 0, reader.HostStatus(r.HostID)),
			Delivery: r.Delivery, Wake: r.Wake != 0, Reacted: r.Reacted != 0,
			MessageID: r.NoticeMessageID.String,
		})
	}
	return out, nil
}

// Atomic reset refusal (R10c, D-R10c-2): one writer transaction checks that
// the owner owns NO nonterminal root incarnation and then deletes every
// message and batch row of that owner. A Reserve cannot interleave: the
// nonterminal scan and the deletion share the transaction, so a root that
// appears before the check refuses the reset and a root that appears after
// survives intact. Refusal writes NOTHING. Mirrors async_job_reset.go; the
// batch model has no wake schedules of its own yet, so the legacy
// CancelAllWakeSchedulesForOwner runs unchanged (D-R10c-5 open decision).

// sessionMessageIDsTx lists the owner's message ids inside the caller's
// transaction (re-declared here after the legacy async_job_reset.go was
// removed; the batch reset and owner-delete cascades share it).
func sessionMessageIDsTx(ctx context.Context, tx *sql.Tx, owner string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM messages WHERE session_id = ?`, owner)
	if err != nil {
		return nil, fmt.Errorf("session reset: list messages: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("session reset: scan message id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("session reset: list messages: %w", err)
	}
	return ids, nil
}

// BatchResetOutcome is what one committed batch reset removed.
type BatchResetOutcome struct {
	MessagesDeleted    int
	NodesDeleted       int64 // batch_nodes rows, all incarnations of the owner
	NoticesVoided      int64 // legacy undelivered session_notices
	SchedulesCancelled int64
}

// batchResetRefusalState names the first nonterminal root incarnation of the
// owner, "stopped but awaiting termination" included: a cancelling
// projection stays nonterminal until the confirmed report (I8).
func batchResetRefusalState(n db.BatchNode) (string, bool) {
	if sessionbatchread.BatchReadTerminal(n.State) {
		return "", false
	}
	return n.State, true
}

// batchResetLiveRoots is the tx-internal refusal scan. Inside the caller's
// transaction the writer connection already holds the write lock, so no
// Reserve can slip a root in between this scan and the deletion.
func batchResetLiveRoots(ctx context.Context, q *db.Queries, owner string) ([]db.BatchNode, error) {
	roots, err := q.BatchHistoryRootsForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	var live []db.BatchNode
	for _, r := range roots {
		if _, non := batchResetRefusalState(r); non {
			live = append(live, r)
		}
	}
	return live, nil
}

// BatchResetOwnerHistory wipes the owner's messages and batch node rows in
// one transaction; see the file doc. DeletedEvents publish only after commit.
func (s *BatchStore) BatchResetOwnerHistory(ctx context.Context, messages message.Service, owner string) (BatchResetOutcome, error) {
	if owner == "" {
		return BatchResetOutcome{}, errors.New("batch reset: owner is required")
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)

	live, err := batchResetLiveRoots(ctx, q, owner)
	if err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: scan live roots: %w", err)
	}
	if len(live) > 0 {
		first := live[0]
		return BatchResetOutcome{}, &BatchResetRefusal{Owner: owner, RootID: first.RootID, ToolCallID: first.ToolCallID.String, State: first.State}
	}

	ids, err := sessionMessageIDsTx(ctx, tx, owner)
	if err != nil {
		return BatchResetOutcome{}, err
	}
	deleted, publish, err := messages.DeleteTx(ctx, tx, owner, ids)
	if err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: delete messages: %w", err)
	}
	now := time.Now().Unix()
	out := BatchResetOutcome{MessagesDeleted: len(deleted)}
	if out.NodesDeleted, err = q.BatchResetDeleteOwnerNodes(ctx, owner); err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: delete batch nodes: %w", err)
	}
	if out.NoticesVoided, err = q.VoidUndeliveredSessionNoticesForOwner(ctx, db.VoidUndeliveredSessionNoticesForOwnerParams{
		UpdatedAt: now, Owner: owner,
	}); err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: void session_notices: %w", err)
	}
	if out.SchedulesCancelled, err = q.CancelAllWakeSchedulesForOwner(ctx, db.CancelAllWakeSchedulesForOwnerParams{
		UpdatedAt: now, OwnerSessionID: owner,
	}); err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: cancel wake schedules: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return BatchResetOutcome{}, fmt.Errorf("batch reset: commit: %w", err)
	}
	publish()
	return out, nil
}

// Batch rerun reconciliation (R10c, D-R10c-3/4): mirrors TruncateForRerun on
// node rows. ONE writer transaction deletes the target and its tail and
// reconciles every selected node against exactly the rows that transaction
// deleted: roots whose announce message was deleted void (plus the legacy
// arm: roots that never named an announce message void by the tail's
// tool_call_id), a done node whose notice message was deleted re-pends, and
// afterwards EVERY selected non-void node inside a voided root ends void —
// including a node the repend just re-pended (the void pass keys off the
// root_id, not the cleared message id). The
// IS NULL guard on the legacy arm keeps a provider-reused call id from
// ever reaching an incarnation that already acked (I5). Executors of voided
// nonterminal roots are stopped only after commit, from the Voided set
// returned here; every entry carries its exact root_id so the stop stays
// incarnation-scoped.
//
// D-R10c-live (R12-F round 4): live old incarnations (pending/starting/
// running/cancelling) are ALSO VOIDED IN THE SAME TRANSACTION. After the
// live-root SELECTs collect them, the caller writes BatchVoidRetiredRoot's
// synthetic select+void (notice_kind='root', notice_state='cancelled',
// notice_cause='stop', notice_sequence=1, notice_ordinal=0, delivery='void',
// wake=0) for exactly those root ids — CHECK-legal because the notice_* SET
// satisfies the notice_selected=1 branch. The write runs BEFORE
// BatchRerunVoidNodesOfVoidedRoots, so the already-selected descendants of
// now-void roots are voided by that existing pass keyed off root
// delivery='void'. Any SQL error rolls back the whole reconcile: the durable
// void is not best-effort here. The Voided set entries remain — they are for
// STOP ROUTING ONLY (the stop goroutine never writes delivery state).

// BatchRerunVoidedNode is one root incarnation the reconciliation voided, as
// it stood when voided. Stop routing must use RootID, never ToolCallID: a
// late completion/ack/stop of this incarnation can never reach a new one.
type BatchRerunVoidedNode struct {
	RootID, NodeID, ToolCallID string
	State                      string
	ChildSessionID, HostID     string
}

// ErrRerunTargetGone means the target message was already deleted (e.g. by a
// concurrent rerun): the reconcile changed nothing.
var ErrRerunTargetGone = errors.New("rerun: target message no longer exists")

// RerunTruncateParams names what a reconcile deletes: the target user message
// plus the tail of messages after it, all owned by Owner.
type RerunTruncateParams struct {
	Owner    string
	TargetID string
	TailIDs  []string
}

// rerunChunk is the IN (...) chunk size for the post-delete reconciliation
// passes (re-declared here after the legacy async_job_rerun.go was removed).
const rerunChunk = 500

// chunkStrings splits ids into consecutive chunks of at most n entries.
func chunkStrings(ids []string, n int) [][]string {
	var out [][]string
	for start := 0; start < len(ids); start += n {
		out = append(out, ids[start:min(start+n, len(ids))])
	}
	return out
}

// nullStrings lifts ids into valid sql.NullStrings for IN (...) queries.
func nullStrings(ids []string) []sql.NullString {
	out := make([]sql.NullString, len(ids))
	for i, id := range ids {
		out[i] = sql.NullString{String: id, Valid: true}
	}
	return out
}

// BatchRerunTruncation is the committed outcome: the message rows actually
// deleted (target included) and the voided root incarnations.
type BatchRerunTruncation struct {
	Deleted []message.Message
	Voided  []BatchRerunVoidedNode
}

func batchRerunVoidedNode(n db.BatchNode) BatchRerunVoidedNode {
	return BatchRerunVoidedNode{RootID: n.RootID, NodeID: n.NodeID, ToolCallID: n.ToolCallID.String, State: n.State, ChildSessionID: n.ChildSessionID.String, HostID: n.HostID}
}

// BatchRerunReconcile runs the batch rerun truncation transaction; see the
// file doc. DeletedEvents publish only after commit. ErrRerunTargetGone rolls
// everything back.
func (s *BatchStore) BatchRerunReconcile(ctx context.Context, messages message.Service, p RerunTruncateParams) (BatchRerunTruncation, error) {
	if p.Owner == "" || p.TargetID == "" {
		return BatchRerunTruncation{}, errors.New("batch rerun: owner and target are required")
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchRerunTruncation{}, fmt.Errorf("batch rerun: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op once committed
	q := db.New(tx)
	now := time.Now().Unix()

	ids := make([]string, 0, len(p.TailIDs)+1)
	ids = append(ids, p.TargetID)
	ids = append(ids, p.TailIDs...)
	deleted, publish, err := messages.DeleteTx(ctx, tx, p.Owner, ids)
	if err != nil {
		return BatchRerunTruncation{}, fmt.Errorf("batch rerun: delete messages: %w", err)
	}
	targetDeleted := false
	deletedIDs := make([]string, 0, len(deleted))
	var tailToolCallIDs []string
	for _, m := range deleted {
		if m.ID == p.TargetID {
			targetDeleted = true
			continue
		}
		deletedIDs = append(deletedIDs, m.ID)
		for _, tc := range m.ToolCalls() {
			tailToolCallIDs = append(tailToolCallIDs, tc.ID)
		}
	}
	if !targetDeleted {
		return BatchRerunTruncation{}, ErrRerunTargetGone
	}

	// Roots first (announce arm, then the legacy arm), so the node passes
	// below can key off the voided roots (D-R10c-4).
	var voided []BatchRerunVoidedNode
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		rows, err := q.BatchRerunVoidRootsByAnnounceMessageIDs(ctx, db.BatchRerunVoidRootsByAnnounceMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: void roots by announce message: %w", err)
		}
		for _, r := range rows {
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
	}
	// Live old incarnations (D-R10c-live): collected first (below), then
	// voided in-tx by batchRerunVoidLiveRoots BEFORE
	// BatchRerunVoidNodesOfVoidedRoots, so the descendants pass sees the
	// roots' delivery='void' and voids their selected nodes too.
	var liveRootIDs []string
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		rows, err := q.BatchRerunLiveRootsByAnnounceMessageIDs(ctx, db.BatchRerunLiveRootsByAnnounceMessageIDsParams{
			Owner: p.Owner, MessageIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: live roots by announce message: %w", err)
		}
		for _, r := range rows {
			liveRootIDs = append(liveRootIDs, r.RootID)
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
	}
	// Legacy arm: never-acked roots only, so a reused tool_call_id cannot
	// reach a later incarnation that already acked (D-R10c-4).
	for _, chunk := range chunkStrings(tailToolCallIDs, rerunChunk) {
		rows, err := q.BatchRerunVoidRootsByToolCallIDs(ctx, db.BatchRerunVoidRootsByToolCallIDsParams{
			UpdatedAt: now, Owner: p.Owner, ToolCallIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: void roots by tool call: %w", err)
		}
		for _, r := range rows {
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
		rows, err = q.BatchRerunLiveRootsByToolCallIDs(ctx, db.BatchRerunLiveRootsByToolCallIDsParams{
			Owner: p.Owner, ToolCallIds: nullStrings(chunk),
		})
		if err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: live roots by tool call: %w", err)
		}
		for _, r := range rows {
			liveRootIDs = append(liveRootIDs, r.RootID)
			voided = batchRerunAddDistinct(voided, batchRerunVoidedNode(r))
		}
	}
	// Void the live roots IN THIS TRANSACTION (R12-F round 4, folds the
	// former post-commit BatchVoidRetiredRoot into reconcile). Only UPDATE
	// (1) — the synthetic select+void — is needed: a live root collected by
	// the SELECTs above cannot already carry notice_selected=1, and any
	// already-selected row of a now-void root (root itself included) is
	// covered by BatchRerunVoidNodesOfVoidedRoots further below, which is
	// why this write precedes it. Best-effort semantics are NOT acceptable
	// in-tx: any error rolls back the whole reconcile.
	if err = batchRerunVoidLiveRoots(ctx, tx, p.Owner, now, liveRootIDs); err != nil {
		return BatchRerunTruncation{}, err
	}
	// Re-pend: a node whose own notice message was deleted re-pends, unless
	// its ROOT was voided — the void pass below then voids it again, keyed by
	// root_id (D-R10c-3: a surviving root's node stays re-pended).
	for _, chunk := range chunkStrings(deletedIDs, rerunChunk) {
		if _, err = q.BatchRerunRependNodesByNoticeMessageIDs(ctx, db.BatchRerunRependNodesByNoticeMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullStrings(chunk),
		}); err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: repend nodes: %w", err)
		}
		// R12-EF fix F2: the session_notices half of the legacy truncation
		// (TruncateForRerun's step 3, async_job_rerun.go) -- session_notices
		// stays live under the flag (R12-D), so a delivered notice whose
		// message the rerun deleted is re-pended for the new branch, and a
		// wake_failed marker of the deleted branch is voided instead.
		if _, err = q.RependSessionNoticesByMessageIDs(ctx, db.RependSessionNoticesByMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullStrings(chunk),
		}); err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: repend session notices: %w", err)
		}
		if _, err = q.VoidWakeFailedNoticesByMessageIDs(ctx, db.VoidWakeFailedNoticesByMessageIDsParams{
			UpdatedAt: now, Owner: p.Owner, MessageIds: nullStrings(chunk),
		}); err != nil {
			return BatchRerunTruncation{}, fmt.Errorf("batch rerun: void wake_failed notices: %w", err)
		}
	}
	// Voided roots ONCE, after the repend loop: void every selected, not-yet-
	// void node inside a voided root regardless of its (cleared) message id.
	if _, err = q.BatchRerunVoidNodesOfVoidedRoots(ctx, db.BatchRerunVoidNodesOfVoidedRootsParams{
		UpdatedAt: now, Owner: p.Owner,
	}); err != nil {
		return BatchRerunTruncation{}, fmt.Errorf("batch rerun: void nodes of voided roots: %w", err)
	}
	// R12-EF fix F3: the question channel of a retired subtree ends void too
	// -- a child_question notice of a void-delivery root is never delivered
	// by the new branch. Runs after the node void above, so roots this very
	// transaction retired are matched; any SQL error rolls the reconcile back.
	if err = batchRerunVoidChildQuestionsOfVoidRoots(ctx, tx, p.Owner, now); err != nil {
		return BatchRerunTruncation{}, err
	}
	if err = tx.Commit(); err != nil {
		return BatchRerunTruncation{}, fmt.Errorf("batch rerun: commit: %w", err)
	}
	publish()
	return BatchRerunTruncation{Deleted: deleted, Voided: voided}, nil
}

// batchRerunVoidLiveRoots voids the collected live root incarnations inside
// the reconcile transaction: one chunked UPDATE per id list, mirroring the
// former BatchVoidRetiredRoot UPDATE (1) exactly — synthetic select+void with
// the CHECK-legal notice identity (kind 'root', state 'cancelled', cause
// 'stop', sequence 1, ordinal 0), delivery='void', wake=0.
func batchRerunVoidLiveRoots(ctx context.Context, tx *sql.Tx, owner string, now int64, rootIDs []string) error {
	for _, chunk := range chunkStrings(rootIDs, rerunChunk) {
		query := `UPDATE batch_nodes SET
		notice_selected = 1, notice_kind = 'root', notice_state = 'cancelled',
		notice_cause = 'stop', notice_sequence = 1, notice_ordinal = 0,
		delivery = 'void', wake = 0, updated_at = ?
	WHERE owner_session_id = ? AND node_id = root_id
		AND notice_selected = 0
		AND state IN ('pending', 'starting', 'running', 'cancelling')
		AND root_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `)`
		args := make([]any, 0, len(chunk)+2)
		args = append(args, now, owner)
		for _, id := range chunk {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("batch rerun: void live roots: %w", err)
		}
	}
	return nil
}

// batchRerunAddDistinct appends n unless a root_id entry already exists: a
// root matching both arms appears once in the stop-routing set.
func batchRerunAddDistinct(set []BatchRerunVoidedNode, n BatchRerunVoidedNode) []BatchRerunVoidedNode {
	for _, v := range set {
		if v.RootID == n.RootID {
			return set
		}
	}
	return append(set, n)
}

// BatchRerunVoidedRoots returns the exact incarnations a committed rerun
// voided. The durable void was already written in the truncation
// transaction; the agent's post-commit stop iterates these ONLY to stop
// executors, and never widens the key to a tool_call_id.
func BatchRerunVoidedRoots(res BatchRerunTruncation) []BatchRerunVoidedNode { return res.Voided }

type BatchStopOwnerTarget struct {
	BatchControlTarget
	RequestID string
	RunnerID  string
	Status    string
}

// BatchStopOwners fences each currently-live root for the requested owners.
func (s *BatchStore) BatchStopOwners(ctx context.Context, owners []string) ([]BatchStopOwnerTarget, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return nil, err
	}
	targets := make([]BatchStopOwnerTarget, 0)
	for _, owner := range owners {
		if owner == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, `UPDATE batch_control_requests
			SET status = 'answered', answer_code = 'accepted',
				answer_sequence = (SELECT root_sequence FROM batch_nodes r WHERE r.root_id = batch_control_requests.root_id AND r.node_id = r.root_id AND r.owner_session_id = batch_control_requests.owner_session_id AND r.host_id = batch_control_requests.host_id AND r.runner_id = batch_control_requests.runner_id),
				answered_at = ?
			WHERE `+psfOwnerStopSQLPredicate("batch_control_requests")+` AND owner_session_id = ? AND node_id = root_id AND operation = 'stop' AND status = 'pending'
				AND EXISTS (SELECT 1 FROM batch_nodes r WHERE r.root_id = batch_control_requests.root_id AND r.node_id = r.root_id AND r.owner_session_id = batch_control_requests.owner_session_id AND r.host_id = batch_control_requests.host_id AND r.runner_id = batch_control_requests.runner_id AND r.state IN ('completed', 'failed', 'cancelled', 'timed_out', 'interrupted', 'skipped') AND r.root_sequence > 0)`, time.Now().Unix(), owner); err != nil {
			return nil, err
		}
		roots, err := q.ListLiveBatchRootsForOwner(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, root := range roots {
			var requestID, runnerID, status string
			err := tx.QueryRowContext(ctx, `SELECT request_id, runner_id, status FROM batch_control_requests WHERE owner_session_id = ? AND host_id = ? AND root_id = ? AND node_id = root_id AND runner_id = ? AND `+psfOwnerStopSQLPredicate("")+` ORDER BY created_at, request_id LIMIT 1`, owner, root.HostID, root.RootID, root.RunnerID).Scan(&requestID, &runnerID, &status)
			if errors.Is(err, sql.ErrNoRows) {
				requestID = psfOwnerStopRequestID()
				result, createErr := tx.ExecContext(ctx, `INSERT INTO batch_control_requests (request_id, owner_session_id, root_id, node_id, host_id, operation, status, answer_code, answer_sequence, created_at, answered_at, runner_id)
					SELECT ?, ?, ?, ?, ?, 'stop', 'pending', '', NULL, ?, NULL, r.runner_id FROM batch_nodes r
					WHERE r.root_id = ? AND r.node_id = r.root_id AND r.owner_session_id = ? AND r.host_id = ? AND r.runner_id = ?`, requestID, owner, root.RootID, root.RootID, root.HostID, time.Now().Unix(), root.RootID, owner, root.HostID, root.RunnerID)
				if createErr != nil {
					return nil, createErr
				}
				n, affectedErr := result.RowsAffected()
				if affectedErr != nil {
					return nil, affectedErr
				}
				if n != 1 {
					return nil, session.ErrBatchProjectionConflict
				}
				runnerID, status = root.RunnerID, "pending"
			} else if err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE batch_nodes SET wake = 0, updated_at = ? WHERE owner_session_id = ? AND host_id = ? AND root_id = ? AND runner_id = ? AND notice_selected = 1 AND wake = 1`, time.Now().Unix(), owner, root.HostID, root.RootID, root.RunnerID); err != nil {
				return nil, err
			}
			targets = append(targets, BatchStopOwnerTarget{BatchControlTarget: BatchControlTarget{Owner: owner, Host: root.HostID, Root: root.RootID, Node: root.RootID}, RequestID: requestID, RunnerID: runnerID, Status: status})
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return targets, nil
}

// BatchAnswerOwnerStop acknowledges the exact durable stop request after a successful cancellation.
func (s *BatchStore) BatchAnswerOwnerStop(ctx context.Context, target BatchStopOwnerTarget) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return err
	}
	request, err := q.GetBatchControlRequest(ctx, db.GetBatchControlRequestParams{RequestID: target.RequestID, OwnerSessionID: target.Owner, HostID: target.Host, RootID: target.Root})
	if err != nil {
		return err
	}
	if !psfIsOwnerStopRequestID(request.RequestID) || request.NodeID != target.Node || request.NodeID != request.RootID || request.RunnerID != target.RunnerID {
		return ErrBatchControlStale
	}
	root, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{RootID: target.Root, OwnerSessionID: target.Owner})
	if err != nil {
		return err
	}
	if root.RunnerID != target.RunnerID || root.HostID != target.Host {
		return ErrBatchControlStale
	}
	if request.Status == "answered" {
		return tx.Commit()
	}
	if root.RootSequence <= 0 {
		return session.ErrBatchProjectionConflict
	}
	if err = session.BatchRequireRow(q.AnswerBatchControlRequest(ctx, db.AnswerBatchControlRequestParams{RequestID: target.RequestID, OwnerSessionID: target.Owner, HostID: target.Host, RootID: target.Root, NodeID: target.Node, RunnerID: target.RunnerID, AnswerCode: "accepted", AnswerSequence: sql.NullInt64{Int64: root.RootSequence, Valid: true}, AnsweredAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}})); err != nil {
		return err
	}
	return tx.Commit()
}

const psfOwnerStopPrefix = "owner_stop:"

func psfOwnerStopRequestID() string {
	return psfOwnerStopPrefix + uuid.NewString()
}

func psfIsOwnerStopRequestID(requestID string) bool {
	return strings.HasPrefix(requestID, psfOwnerStopPrefix)
}

func psfOwnerStopSQLPredicate(alias string) string {
	column := "request_id"
	if alias != "" {
		column = alias + ".request_id"
	}
	return column + " GLOB '" + psfOwnerStopPrefix + "*'"
}
