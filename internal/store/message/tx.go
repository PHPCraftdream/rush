package message

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/store/db"
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

// InterruptedAssistantCandidate is one row of
// ListCandidateInterruptedAssistantSessions: a session whose chronologically
// last message is an unfinished assistant message, i.e. a candidate for
// startup recovery (task #774).
type InterruptedAssistantCandidate struct {
	SessionID string
	MessageID string
	// ParentSessionID is empty for a top-level session, matching
	// session.Session.ParentSessionID's convention.
	ParentSessionID string
}

// ListCandidateInterruptedAssistantSessions implementation: thin wrapper
// around the sqlc-generated query. See the Service interface doc comment and
// the query's own doc comment (internal/db/sql/messages.sql) for the full
// rationale and correctness notes.
func (s *service) ListCandidateInterruptedAssistantSessions(ctx context.Context) ([]InterruptedAssistantCandidate, error) {
	rows, err := s.qRead.ListCandidateInterruptedAssistantSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]InterruptedAssistantCandidate, len(rows))
	for i, row := range rows {
		out[i] = InterruptedAssistantCandidate{
			SessionID:       row.SessionID,
			MessageID:       row.MessageID,
			ParentSessionID: row.ParentSessionID.String,
		}
	}
	return out, nil
}

// StampInterruptedAssistantIfStillLast implementation: thin wrapper around
// the sqlc-generated conditional query. See the Service interface doc
// comment and the query's own doc comment (internal/db/sql/messages.sql) for
// the full rationale.
//
// msg is expected to already carry the process-restart Finish part (the
// caller calls AddFinish before this) -- this only marshals and writes it,
// conditioned on the row still being both unfinished and the session's last
// message as of this single statement.
func (s *service) StampInterruptedAssistantIfStillLast(ctx context.Context, sessionID string, msg Message) (bool, error) {
	parts, err := marshalParts(msg.Parts)
	if err != nil {
		return false, err
	}
	finishedAt := sql.NullInt64{}
	if finish := msg.FinishPart(); finish != nil && !finish.Partial {
		finishedAt.Int64 = finish.Time
		finishedAt.Valid = true
	}
	rowsAffected, err := s.q.StampInterruptedAssistantIfStillLast(ctx, db.StampInterruptedAssistantIfStillLastParams{
		Parts:      string(parts),
		FinishedAt: finishedAt,
		ID:         msg.ID,
		SessionID:  sessionID,
	})
	if err != nil {
		return false, err
	}
	if rowsAffected == 0 {
		// Candidate went stale between discovery and this write: either
		// superseded by a newer message, or finished concurrently by its
		// live owner. Not an error -- the caller skips and does not retry.
		return false, nil
	}
	// Terminal write: PublishMustDeliver, matching Update's terminal-write
	// delivery semantics (a momentarily full subscriber buffer must not
	// silently eat this state).
	msg.UpdatedAt = time.Now().Unix()
	s.PublishMustDeliver(ctx, pubsub.UpdatedEvent, msg.Clone())
	return true, nil
}

// message_tx_querier.go is additive (package PD11C): Querier-parameterised
// counterparts of CreateTx/UpdateTx. The fused batch transactions
// (internal/session/batch_*.go) hand the service their binder-backed
// *db.Queries so the message INSERT/UPDATE executes through the batch
// prepared-statement cache instead of preparing on every execution. They are
// deliberately NOT part of the Service interface: existing implementers and
// test fakes keep compiling, and batch call sites type-assert to the optional
// capability and fall back to CreateTx/UpdateTx when it is absent — identical
// behavior through the raw tx.

// CreateTxQ is CreateTx with the INSERT running on the caller-supplied
// Querier (the batch transaction's binder-backed Queries) instead of a bare
// *sql.Tx. Publishing stays with the caller via PublishCreated, exactly as
// CreateTx.
func (s *service) CreateTxQ(ctx context.Context, q db.Querier, sessionID string, params CreateMessageParams) (Message, error) {
	return s.createWith(ctx, q, sessionID, params)
}

// UpdateTxQ is UpdateTx with the UPDATE running on the caller-supplied
// Querier. Same contract as UpdateTx: call the returned func ONLY after the
// caller's transaction commits; it is nil on error.
func (s *service) UpdateTxQ(ctx context.Context, q db.Querier, msg Message) (func(), error) {
	rowsAffected, partialCheckpoint, err := s.updateWith(ctx, q, msg)
	if err != nil {
		return nil, err
	}
	msg.UpdatedAt = time.Now().Unix()
	return func() { s.publishUpdate(ctx, msg, rowsAffected, partialCheckpoint) }, nil
}
