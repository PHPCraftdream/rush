package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

// ErrBatchAnnouncementLost means another ack/inline consumer won or the
// addressed incarnation disappeared. No history is inserted by the loser.
var ErrBatchAnnouncementLost = errors.New("batch announcement election lost")

// BatchAnnounceStarted commits started history and root announcement together.
// It does not require a live state, delete a failed reservation, or dispatch.
func (s *BatchStore) BatchAnnounceStarted(ctx context.Context, messages message.Service, owner, root string, params message.CreateMessageParams) (message.Message, error) {
	// See batch_tool_result.go: prepare before the tx holds the pool's only
	// connection.
	if err := db.PdaBatchEnsure(ctx, s.writer); err != nil {
		return message.Message{}, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return message.Message{}, err
	}
	defer tx.Rollback()
	q := db.PdaBatchTxQueries(ctx, s.writer, tx)
	// Acquire the writer and elect before history insertion. The placeholder is
	// never externally visible; the actual message identity replaces it in tx.
	n, err := q.BatchOutboxAnnounce(ctx, db.BatchOutboxAnnounceParams{Owner: owner, RootID: root, MessageID: sql.NullString{String: "reserved", Valid: true}, UpdatedAt: time.Now().Unix()})
	if err != nil {
		return message.Message{}, err
	}
	if n != 1 {
		return message.Message{}, ErrBatchAnnouncementLost
	}
	msg, err := pdcCreateTx(ctx, q, tx, messages, owner, params)
	if err != nil {
		return message.Message{}, err
	}
	if err = batchRequireRow(q.BatchOutboxSetAnnouncementMessage(ctx, db.BatchOutboxSetAnnouncementMessageParams{Owner: owner, RootID: root, MessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix()})); err != nil {
		return message.Message{}, err
	}
	if err = tx.Commit(); err != nil {
		return message.Message{}, err
	}
	messages.PublishCreated(msg)
	return msg, nil
}
