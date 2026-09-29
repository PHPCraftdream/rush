package message

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

// deleteTxChunk bounds the IN-list of one DELETE statement (well under
// SQLite's bound-variable limit).
const deleteTxChunk = 500

// DeleteTx implements Service.DeleteTx: the transaction-aware, unconditional
// batch delete Rerun truncation runs inside its single writer transaction.
// Nothing is published here; the returned publish func bumps the session's
// delete generation and publishes one DeletedEvent per deleted row, and must
// be called only after the caller's transaction committed.
func (s *service) DeleteTx(ctx context.Context, tx *sql.Tx, sessionID string, ids []string) ([]Message, func(), error) {
	noop := func() {}
	if len(ids) == 0 {
		return nil, noop, nil
	}
	q := db.New(tx)
	var deleted []Message
	for start := 0; start < len(ids); start += deleteTxChunk {
		end := min(start+deleteTxChunk, len(ids))
		rows, err := q.DeleteSessionMessagesByIDs(ctx, db.DeleteSessionMessagesByIDsParams{
			SessionID: sessionID, Ids: ids[start:end],
		})
		if err != nil {
			return nil, noop, fmt.Errorf("message service: delete tx: %w", err)
		}
		for _, row := range rows {
			msg, err := s.fromDBItem(row)
			if err != nil {
				return nil, noop, fmt.Errorf("message service: delete tx: decode %s: %w", row.ID, err)
			}
			deleted = append(deleted, msg)
		}
	}
	publish := func() {
		for _, msg := range deleted {
			msg.DeleteGeneration = s.bumpDeleteGeneration(msg.SessionID)
			s.PublishMustDeliver(ctx, pubsub.DeletedEvent, msg.Clone())
		}
	}
	return deleted, publish, nil
}
