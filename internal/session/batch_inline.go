package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// BatchInlineAnnouncement is an elected one-item completion, never stop intent.
type BatchInlineAnnouncement struct {
	Msg          message.Message
	DeliveryRows int64
}

// BatchAnnounceInlineResult elects only an unannounced selected terminal root
// with exactly one task. Ack, history and delivery consumption are atomic.
// A pull/ack/inline loser inserts no history. Call only with the actual elected
// completion envelope, not with a generic control/stop-intent answer.
func (s *BatchStore) BatchAnnounceInlineResult(ctx context.Context, messages message.Service, owner, root string, params message.CreateMessageParams) (BatchInlineAnnouncement, error) {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return BatchInlineAnnouncement{}, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	_, err = q.BatchOutboxInlineElect(ctx, db.BatchOutboxInlineElectParams{Owner: owner, RootID: root, UpdatedAt: time.Now().Unix()})
	if errors.Is(err, sql.ErrNoRows) {
		return BatchInlineAnnouncement{}, ErrBatchAnnouncementLost
	}
	if err != nil {
		return BatchInlineAnnouncement{}, err
	}
	msg, err := messages.CreateTx(ctx, tx, owner, params)
	if err != nil {
		return BatchInlineAnnouncement{}, err
	}
	id := sql.NullString{String: msg.ID, Valid: true}
	if err = batchRequireRow(q.BatchOutboxAnnounce(ctx, db.BatchOutboxAnnounceParams{Owner: owner, RootID: root, MessageID: id, UpdatedAt: time.Now().Unix()})); err != nil {
		return BatchInlineAnnouncement{}, err
	}
	if err = batchRequireRow(q.BatchOutboxNameMessage(ctx, db.BatchOutboxNameMessageParams{Owner: owner, RootID: root, NodeID: root, MessageID: id, UpdatedAt: time.Now().Unix()})); err != nil {
		return BatchInlineAnnouncement{}, err
	}
	if err = tx.Commit(); err != nil {
		return BatchInlineAnnouncement{}, err
	}
	messages.PublishCreated(msg)
	return BatchInlineAnnouncement{Msg: msg, DeliveryRows: 1}, nil
}
