package session

import (
	"context"
	"database/sql"
	"fmt"
)

// FinalizationTransaction serializes candidate acceptance with persisted cancel.
// The caller must already hold exclusive session ownership and its scope seal.
type FinalizationTransaction interface {
	WithFinalizationTx(context.Context, string, func(*sql.Tx) error) error
}

func (s *service) WithFinalizationTx(ctx context.Context, id string, accept func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finalization publication: %w", err)
	}
	defer tx.Rollback()
	row, err := s.q.WithTx(tx).GetSessionByID(ctx, id)
	if err != nil {
		return fmt.Errorf("read finalization owner: %w", err)
	}
	if row.CancelRequested != 0 {
		return context.Canceled
	}
	if err := accept(tx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finalization publication: %w", err)
	}
	return nil
}

var _ FinalizationTransaction = (*service)(nil)
