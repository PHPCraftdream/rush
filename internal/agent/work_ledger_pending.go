package agent

import (
	"context"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// durableDeliveryStatus classifies a job that is gone from the in-memory
// map by its durable async_jobs row (B1). Called WITHOUT l.mu (no DB I/O
// under the ledger lock) and bounded by a short timeout; a row that left the
// map only moves pending -> done, so the answer is true when read. A nil store, a read
// error, a still-running row, or a row whose delivery state does not answer
// the question ('none'/'void') returns nil -- the caller keeps its plain
// not-found error, which is honest in those cases.
func (l *workLedger) durableDeliveryStatus(owner, jobID string) error {
	if l.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	row, err := l.store.Get(ctx, owner, jobID)
	if err != nil || row.State == "running" {
		return nil
	}
	switch row.Delivery {
	case "done":
		return &tools.JobDeliveryStatusError{JobID: jobID, Delivered: true}
	case "pending":
		return &tools.JobDeliveryStatusError{JobID: jobID}
	}
	return nil
}
