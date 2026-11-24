package sessguard

import (
	"context"
	"database/sql"
	"fmt"
)

// Guard inspects persisted finalization scopes using the writer and optional reader.
type Guard struct {
	writer *sql.DB
	reader *sql.DB
}

// NewGuard binds the writer and optional read database used for scope inspection.
func NewGuard(writer, reader *sql.DB) *Guard {
	return &Guard{writer: writer, reader: reader}
}

// HasFinalizationScope reports whether an owner has a persisted finalization scope.
func (g *Guard) HasFinalizationScope(ctx context.Context, owner string) (bool, error) {
	reader := g.reader
	if reader == nil {
		reader = g.writer
	}
	if reader != g.writer {
		var exists int
		err := reader.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, owner).Scan(&exists)
		if err != nil {
			return false, fmt.Errorf("inspect persisted finalization scope: %w", err)
		}
		if exists != 0 {
			return true, nil
		}
	}
	var exists int
	err := g.writer.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_agent_finalizations WHERE session_id=?)`, owner).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("inspect persisted finalization scope: %w", err)
	}
	return exists != 0, nil
}
