package message

// message_tx_querier.go is additive (package PD11C): Querier-parameterised
// counterparts of CreateTx/UpdateTx. The fused batch transactions
// (internal/session/batch_*.go) hand the service their binder-backed
// *db.Queries so the message INSERT/UPDATE executes through the batch
// prepared-statement cache instead of preparing on every execution. They are
// deliberately NOT part of the Service interface: existing implementers and
// test fakes keep compiling, and batch call sites type-assert to the optional
// capability and fall back to CreateTx/UpdateTx when it is absent — identical
// behavior through the raw tx.

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
)

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
