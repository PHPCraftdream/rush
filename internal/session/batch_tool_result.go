package session

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
)

var ErrBatchToolResultIdentity = errors.New("batch tool result: invalid delivery identity")

// BatchToolResultIdentity fences one consumed root incarnation.
type BatchToolResultIdentity struct {
	Owner, RootID, NodeID, HostID, RunnerID, ToolCallID string
}

// BatchConsumeToolResult atomically persists the actual tool message and consumes
// the matching root. A lost incarnation CAS is a successful ordinary result write.
func (s *BatchStore) BatchConsumeToolResult(ctx context.Context, messages message.Service, owner string, identity BatchToolResultIdentity, params message.CreateMessageParams) (message.Message, bool, error) {
	if owner == "" || owner != identity.Owner || identity.RootID == "" || identity.NodeID != identity.RootID || identity.HostID == "" || identity.RunnerID == "" || identity.ToolCallID == "" || params.Role != message.Tool {
		return message.Message{}, false, ErrBatchToolResultIdentity
	}
	// Prepare the batch statement cache BEFORE opening the tx: the writer pool
	// has exactly one connection, and preparing while the tx holds it would
	// deadlock (PdaBatchTxQueries itself never prepares; on a miss it falls
	// back to raw SQL, which would only happen if a pool release raced with
	// the Ensure below).
	if err := db.PdaBatchEnsure(ctx, s.writer); err != nil {
		return message.Message{}, false, err
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return message.Message{}, false, err
	}
	defer tx.Rollback()
	msg, err := messages.CreateTx(ctx, tx, owner, params)
	if err != nil {
		return message.Message{}, false, err
	}
	q := db.PdaBatchTxQueries(ctx, s.writer, tx)
	rows, err := q.BatchOutboxConsumeWithToolResult(ctx, db.BatchOutboxConsumeWithToolResultParams{
		MessageID: sql.NullString{String: msg.ID, Valid: true}, UpdatedAt: time.Now().Unix(),
		Owner: owner, RootID: identity.RootID, NodeID: identity.NodeID, HostID: identity.HostID,
		RunnerID: identity.RunnerID, ToolCallID: sql.NullString{String: identity.ToolCallID, Valid: true},
	})
	if err != nil {
		return message.Message{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return message.Message{}, false, err
	}
	messages.PublishCreated(msg)
	return msg, rows == 1, nil
}
