package sessguard

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/store/db"
)

// FinalizationTransaction serializes candidate acceptance with persisted cancel.
type FinalizationTransaction struct {
	db      *sql.DB
	queries *db.Queries
}

// NewFinalizationTransaction binds the database and generated query layer used
// for finalization publication.
func NewFinalizationTransaction(database *sql.DB, queries *db.Queries) *FinalizationTransaction {
	return &FinalizationTransaction{db: database, queries: queries}
}

// WithFinalizationTx publishes accepted work only while the owner remains uncanceled.
func (f *FinalizationTransaction) WithFinalizationTx(ctx context.Context, id string, accept func(*sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin finalization publication: %w", err)
	}
	defer tx.Rollback()
	row, err := f.queries.WithTx(tx).GetSessionByID(ctx, id)
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
