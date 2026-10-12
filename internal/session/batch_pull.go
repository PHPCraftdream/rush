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
	rows, err := db.New(s.writer).BatchOutboxPending(ctx, owner)
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
	n, err := q.BatchOutboxPull(ctx, db.BatchOutboxPullParams{Owner: owner, RootID: root, NodeID: node, UpdatedAt: time.Now().Unix()})
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
