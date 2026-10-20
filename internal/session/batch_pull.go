package session

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// batchOutboxPendingVoidAwareRow is the only identity the pull loop needs.
type batchOutboxPendingVoidAwareRow struct{ RootID, NodeID string }

// batchOutboxPendingVoidAware mirrors the canonical BatchOutboxPending
// predicate (notice_selected=1, delivery='pending', root announced=1, same
// ORDER BY) plus the R12-F root-void exclusion (r.delivery != 'void'): a
// root durably retired by a rerun must never surface its selected notices.
// Expressed Go-side because this fork has no pinned sqlc regeneration path;
// fold back into internal/db/sql/batch_outbox.sql when regeneration is
// available.
func batchOutboxPendingVoidAware(ctx context.Context, dbtx db.DBTX, owner string) ([]batchOutboxPendingVoidAwareRow, error) {
	rows, err := dbtx.QueryContext(ctx, `SELECT n.root_id, n.node_id FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.owner_session_id = ? AND n.notice_selected = 1
  AND n.delivery = 'pending' AND r.announced = 1 AND r.delivery != 'void'
ORDER BY r.created_at, n.root_id, n.notice_sequence, n.notice_ordinal, n.node_id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchOutboxPendingVoidAwareRow
	for rows.Next() {
		var row batchOutboxPendingVoidAwareRow
		if err := rows.Scan(&row.RootID, &row.NodeID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// batchOutboxPullVoidAware is the canonical BatchOutboxPull CAS (root gate,
// oldest-outstanding-order guard) plus the R12-F root-void exclusion: the
// extra NOT EXISTS refuses to deliver a notice whose root was durably
// retired by a rerun (delivery='void'). Expressed Go-side because this fork
// has no pinned sqlc regeneration path; fold back into
// internal/db/sql/batch_outbox.sql when regeneration is available.
const batchOutboxPullVoidAware = `UPDATE batch_nodes SET delivery = 'done', updated_at = ?
WHERE batch_nodes.owner_session_id = ? AND batch_nodes.root_id = ? AND batch_nodes.node_id = ?
  AND batch_nodes.notice_selected = 1 AND batch_nodes.delivery = 'pending'
  AND EXISTS (SELECT 1 FROM batch_nodes r WHERE r.node_id = batch_nodes.root_id AND r.announced = 1)
  AND NOT EXISTS (SELECT 1 FROM batch_nodes r WHERE r.node_id = batch_nodes.root_id AND r.owner_session_id = batch_nodes.owner_session_id AND r.delivery = 'void')
  AND NOT EXISTS (SELECT 1 FROM batch_nodes earlier
    WHERE earlier.root_id = batch_nodes.root_id AND earlier.notice_selected = 1 AND earlier.delivery = 'pending'
      AND (earlier.notice_sequence < batch_nodes.notice_sequence
        OR (earlier.notice_sequence = batch_nodes.notice_sequence AND earlier.notice_ordinal < batch_nodes.notice_ordinal)
        OR (earlier.notice_sequence = batch_nodes.notice_sequence AND earlier.notice_ordinal = batch_nodes.notice_ordinal AND earlier.node_id < batch_nodes.node_id)))
RETURNING *`

// scanBatchNodeRow scans RETURNING * in table column order (the generated
// BatchOutboxPull scan order), so it must be kept in sync with the
// batch_nodes schema until the query folds back into the .sql file.
func scanBatchNodeRow(row *sql.Row) (db.BatchNode, error) {
	var i db.BatchNode
	err := row.Scan(
		&i.NodeID,
		&i.RootID,
		&i.OwnerSessionID,
		&i.HostID,
		&i.ParentID,
		&i.Ordinal,
		&i.Kind,
		&i.ToolCallID,
		&i.InputHash,
		&i.Announced,
		&i.AnnounceMessageID,
		&i.OriginCli,
		&i.Mode,
		&i.OnFail,
		&i.Notify,
		&i.MaxParallel,
		&i.TaskKind,
		&i.TaskGroup,
		&i.TaskPayload,
		&i.TimeoutNs,
		&i.ChildSessionID,
		&i.State,
		&i.Cause,
		&i.Token,
		&i.Attempts,
		&i.ControlState,
		&i.ControlError,
		&i.ControlPending,
		&i.OutcomeState,
		&i.OutcomeCode,
		&i.OutcomeSummary,
		&i.ResponseEnvelopeRef,
		&i.RootSequence,
		&i.ProjectionSequence,
		&i.NoticeSelected,
		&i.NoticeKind,
		&i.NoticeState,
		&i.NoticeCause,
		&i.NoticeSequence,
		&i.NoticeOrdinal,
		&i.NoticePending,
		&i.NoticeActive,
		&i.NoticeCompleted,
		&i.NoticeFailed,
		&i.NoticeCancelled,
		&i.NoticeTimedOut,
		&i.NoticeInterrupted,
		&i.NoticeSkipped,
		&i.Delivery,
		&i.NoticeMessageID,
		&i.Wake,
		&i.Reacted,
		&i.WakeAttempts,
		&i.ReactedFailed,
		&i.CreatedAt,
		&i.UpdatedAt,
		&i.RunnerID,
		&i.ResponseEnvelope,
	)
	return i, err
}

// BatchNoticeRow is a selected notice, not an inferred terminal notification.
// RootID and NodeID identify the incarnation; ToolCallID belongs to the root.
type BatchNoticeRow struct {
	RootID, NodeID, ToolCallID                                                    string
	Kind, State, Cause, OutcomeCode, OutcomeSummary, ResponseEnvelopeRef          string
	Sequence, Ordinal                                                             int64
	Pending, Active, Completed, Failed, Cancelled, TimedOut, Interrupted, Skipped int64
	OriginCLI                                                                     bool
}

// BatchPulledNotice names the exact durable delivery consumed by a pull.
type BatchPulledNotice struct {
	Message        message.Message
	Wake           bool
	RootID, NodeID string
}

func batchNoticeRow(n, root db.BatchNode) BatchNoticeRow {
	return BatchNoticeRow{
		RootID: n.RootID, NodeID: n.NodeID, ToolCallID: root.ToolCallID.String,
		Kind: n.NoticeKind.String, State: n.NoticeState.String, Cause: n.NoticeCause.String,
		OutcomeCode: n.OutcomeCode, OutcomeSummary: n.OutcomeSummary, ResponseEnvelopeRef: n.ResponseEnvelopeRef.String,
		Sequence: n.NoticeSequence.Int64, Ordinal: n.NoticeOrdinal.Int64,
		Pending: n.NoticePending, Active: n.NoticeActive, Completed: n.NoticeCompleted, Failed: n.NoticeFailed,
		Cancelled: n.NoticeCancelled, TimedOut: n.NoticeTimedOut, Interrupted: n.NoticeInterrupted, Skipped: n.NoticeSkipped,
		OriginCLI: root.OriginCli != 0,
	}
}

// BatchPullNotices consumes selected notices in root sequence/ordinal order.
// A failed row remains pending, blocks later notices of that root, and does
// not fail the caller's turn. The CAS rechecks announcement and ordering.
func (s *BatchStore) BatchPullNotices(ctx context.Context, messages message.Service, owner string, build func(BatchNoticeRow) message.CreateMessageParams) ([]BatchPulledNotice, error) {
	// Go-side predicate: BatchOutboxPending + the root-void exclusion (see
	// batchOutboxPendingVoidAware; fold back into batch_outbox.sql when sqlc
	// regeneration is available).
	rows, err := batchOutboxPendingVoidAware(ctx, s.writer, owner)
	if err != nil {
		return nil, err
	}
	var out []BatchPulledNotice
	for _, row := range rows {
		pulled, ok, err := s.batchPullOneNotice(ctx, messages, owner, row.RootID, row.NodeID, build)
		if err != nil {
			slog.Warn("Batch notice pull failed; row stays pending", "owner", owner, "node", row.NodeID, "err", err)
			continue
		}
		if ok {
			out = append(out, pulled)
		}
	}
	return out, nil
}

func (s *BatchStore) batchPullOneNotice(ctx context.Context, messages message.Service, owner, root, node string, build func(BatchNoticeRow) message.CreateMessageParams) (BatchPulledNotice, bool, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchPulledNotice{}, false, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	// Go-side CAS: BatchOutboxPull + the root-void NOT EXISTS (see
	// batchOutboxPullVoidAware; fold back into batch_outbox.sql when sqlc
	// regeneration is available). Same statement, same RETURNING * scan.
	n, err := scanBatchNodeRow(tx.QueryRowContext(ctx, batchOutboxPullVoidAware, time.Now().Unix(), owner, root, node))
	if errors.Is(err, sql.ErrNoRows) {
		return BatchPulledNotice{}, false, nil
	}
	if err != nil {
		return BatchPulledNotice{}, false, err
	}
	r, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{OwnerSessionID: owner, RootID: root})
	if err != nil {
		return BatchPulledNotice{}, false, err
	}
	// N (package N): the selected notice's content must be the job's real
	// result. Structural settlement (internal/jobbatch/tree_apply.go) does
	// not copy a task outcome into the structural root, so an ordinary
	// one-task root settles with an empty summary while its sole task
	// child carries the output; the sync path projects root -> sole child
	// the same way (batch_adapters.go BatchDeliveryOneItemCompletion).
	// Multi-task roots, nested structural children, and non-root selected
	// notices are never borrowed from: they keep the row's own summary.
	projected, err := batchProjectSelectedNoticeSummary(ctx, tx, owner, n)
	if err != nil {
		return BatchPulledNotice{}, false, err
	}
	n.OutcomeSummary = projected
	msg, err := messages.CreateTx(ctx, tx, owner, build(batchNoticeRow(n, r)))
	if err != nil {
		return BatchPulledNotice{}, false, err
	}
	if err = batchRequireRow(q.BatchOutboxNameMessage(ctx, db.BatchOutboxNameMessageParams{Owner: owner, RootID: root, NodeID: node, MessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix()})); err != nil {
		return BatchPulledNotice{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return BatchPulledNotice{}, false, err
	}
	messages.PublishCreated(msg)
	return BatchPulledNotice{Message: msg, Wake: n.Wake != 0, RootID: root, NodeID: node}, true, nil
}

// batchProjectSelectedNoticeSummary resolves the summary a selected notice
// carries. A non-root selected notice (a NotifyEach task) and any root that
// already carries its own summary pass through unchanged. An ordinary
// ONE-TASK root with an empty summary resolves from its sole direct task
// child: the state/cause on the row stay authoritative (a stopped or failed
// root is never reclassified by the child), only the content is projected.
// The read happens inside the caller's pull transaction, after the delivery
// CAS, so a read error rolls the whole delivery back and the row stays
// pending. Everything comes from durable rows; no process-local cache.
// Only the textual summary is projected: the model-visible async completion
// formatter is text-only (agent_notice_pull.go buildBatchNoticeMessageParams
// -> coordinator_background.go FormatAsyncCompletion), and today's
// detached/escaped paths carry text-only results, so a media-only detached
// result is not reachable.
func batchProjectSelectedNoticeSummary(ctx context.Context, dbtx db.DBTX, owner string, n db.BatchNode) (string, error) {
	if n.NodeID != n.RootID || n.OutcomeSummary != "" {
		return n.OutcomeSummary, nil
	}
	children, err := batchDirectChildOutcomes(ctx, dbtx, owner, n.RootID, n.NodeID)
	if err != nil {
		return "", err
	}
	// More than one child, or a structural (non-task) child: the root's
	// own outcome is the only honest aggregate; never borrow one
	// arbitrary task's result for a multi-task batch.
	if len(children) != 1 || children[0].Kind != "task" {
		return n.OutcomeSummary, nil
	}
	return children[0].Summary, nil
}

// batchChildOutcome is one direct child's kind and outcome summary.
type batchChildOutcome struct{ Kind, Summary string }

// batchDirectChildOutcomes reads every direct child of one node in a single
// pass. The writer pool has exactly one connection
// (internal/db/connect.go SetMaxOpenConns(1)), so the rows MUST be fully
// read and closed before the caller runs its next statement; the deferred
// Close inside this helper guarantees that.
func batchDirectChildOutcomes(ctx context.Context, dbtx db.DBTX, owner, root, node string) ([]batchChildOutcome, error) {
	rows, err := dbtx.QueryContext(ctx, `SELECT kind, outcome_summary FROM batch_nodes
WHERE owner_session_id = ? AND root_id = ? AND parent_id = ?`, owner, root, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []batchChildOutcome
	for rows.Next() {
		var c batchChildOutcome
		if err := rows.Scan(&c.Kind, &c.Summary); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
