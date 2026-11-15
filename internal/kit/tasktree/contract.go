package tasktree

import (
	"context"
	"fmt"
)

type Receipt struct {
	RequestID         RequestID `json:"request_id"`
	Actor             Actor     `json:"actor"`
	Fingerprint       string    `json:"fingerprint"`
	CommittedRevision Revision  `json:"committed_revision"`
	Delta             Delta     `json:"delta"`
}

type Envelope struct {
	Revision Revision              `json:"revision"`
	Snapshot Snapshot              `json:"snapshot"`
	Receipts map[RequestID]Receipt `json:"receipts"`
}

type MutationReply struct {
	Receipt  Receipt     `json:"receipt"`
	Replayed bool        `json:"replayed"`
	Summary  Summary     `json:"summary"`
	Created  []NodeBrief `json:"created,omitempty"`
}

// Store atomically publishes detached snapshots and receipts.
type Store interface {
	Load(context.Context, TreeKey) (Envelope, error)
	Commit(context.Context, TreeKey, Revision, Envelope) error
}

// InfrastructureError distinguishes storage failures from correctable input.
type InfrastructureError struct {
	Operation string
	Err       error
}

func (e *InfrastructureError) Error() string {
	return fmt.Sprintf("task tree %s: %v", e.Operation, e.Err)
}

func (e *InfrastructureError) Unwrap() error {
	return e.Err
}

// CommitUnknownError requires reconciliation with the original request ID.
type CommitUnknownError struct {
	Err error
}

func (e *CommitUnknownError) Error() string {
	return fmt.Sprintf("task tree commit outcome unknown: %v", e.Err)
}

func (e *CommitUnknownError) Unwrap() error {
	return e.Err
}
