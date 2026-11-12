package message

import (
	"context"
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/db"
)

// FinalizationReader loads the candidate in the seal's writer transaction.
type FinalizationReader interface {
	GetFinalizationCandidateTx(context.Context, *sql.Tx, string) (Message, error)
}

func (s *service) GetFinalizationCandidateTx(ctx context.Context, tx *sql.Tx, id string) (Message, error) {
	row, err := db.New(tx).GetMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	return s.fromDBItem(row)
}

var _ FinalizationReader = (*service)(nil)
