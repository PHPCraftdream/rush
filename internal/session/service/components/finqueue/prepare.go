package finqueue

import (
	"context"
	"encoding/json"
)

// Preparation owns a detached call-data snapshot and its verified queue snapshot.
// Service-specific restore and consume operations are supplied as typed ports.
type Preparation[TData, TSnapshot any] struct {
	rowID    string
	frozen   []byte
	snapshot TSnapshot
}

// Prepare snapshots call data before validation and restores without consuming.
func Prepare[TData, TSnapshot any](ctx context.Context, rowID string, data TData, clone func([]byte) (TData, error), restore func(context.Context, string, TData, bool) (TSnapshot, error), changed error) (*Preparation[TData, TSnapshot], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if rowID == "" {
		return nil, changed
	}
	frozen, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	copy, err := clone(frozen)
	if err != nil {
		return nil, err
	}
	snapshot, err := restore(ctx, rowID, copy, false)
	if err != nil {
		return nil, err
	}
	return &Preparation[TData, TSnapshot]{rowID: rowID, frozen: frozen, snapshot: snapshot}, nil
}

// FrozenData returns detached decoded preparation input.
func (p *Preparation[TData, TSnapshot]) FrozenData(decode func([]byte) (TData, error)) (TData, error) {
	if p == nil {
		var zero TData
		return zero, nil
	}
	return decode(p.frozen)
}

// Snapshot returns the validated service snapshot without exposing mutable call data.
func (p *Preparation[TData, TSnapshot]) Snapshot() TSnapshot {
	if p == nil {
		var zero TSnapshot
		return zero
	}
	return p.snapshot
}

// RowID identifies the prepared queue row.
func (p *Preparation[TData, TSnapshot]) RowID() string {
	if p == nil {
		return ""
	}
	return p.rowID
}
