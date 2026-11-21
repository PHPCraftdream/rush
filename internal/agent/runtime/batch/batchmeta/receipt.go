package batchmeta

import (
	"context"
	"errors"
	"sync"

	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatch"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Receipt binds a tool call to its guarded persistence operation.
type Receipt struct {
	identity sessionbatch.BatchToolResultIdentity
	// persist fuses the actual tool-result message with the guarded root
	// consumption CAS in ONE transaction (sessionbatch.BatchConsumeToolResult).
	persist func(context.Context, message.CreateMessageParams) (message.Message, bool, error)
}

// NewReceipt binds an identity to its guarded persistence operation.
func NewReceipt(identity sessionbatch.BatchToolResultIdentity, persist func(context.Context, message.CreateMessageParams) (message.Message, bool, error)) Receipt {
	return Receipt{identity: identity, persist: persist}
}

// Identity identifies the tool call and root bound to this receipt.
func (r Receipt) Identity() sessionbatch.BatchToolResultIdentity { return r.identity }

// Persist writes the tool result and consumes the root in one transaction.
func (r Receipt) Persist(ctx context.Context, params message.CreateMessageParams) (message.Message, bool, error) {
	return r.persist(ctx, params)
}

// ReceiptSink holds receipts until the turn consumes their tool results.
// Its zero value is ready to use.
type ReceiptSink struct {
	mu       sync.Mutex
	receipts map[string]Receipt
}

// Add registers a receipt and rejects a duplicate tool call ID.
func (s *ReceiptSink) Add(receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receipts == nil {
		receipts := make(map[string]Receipt)
		s.receipts = receipts
	}
	id := receipt.identity.ToolCallID
	if _, exists := s.receipts[id]; exists {
		return errors.New("batch tool result: duplicate call receipt")
	}
	s.receipts[id] = receipt
	return nil
}

// Take removes and returns the receipt for callID.
func (s *ReceiptSink) Take(callID string) (Receipt, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[callID]
	delete(s.receipts, callID)
	return r, ok
}
