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

// FinalizationWriter stores a provisional candidate in the caller's writer
// transaction so the message row and the finalization record commit together.
type FinalizationWriter interface {
	StoreFinalizationCandidateTx(context.Context, *sql.Tx, Message, FinalizationIdentity) (stored Message, hash string, publish func(), err error)
}

// StoreFinalizationCandidateTx writes m (an unfinished candidate carrying id)
// through the checkpoint-generation fence and returns the row as stored with the
// hash of its stored parts. Zero rows written (a newer checkpoint generation, a
// terminal row, a deleted row) is a changed candidate, never a silent success.
// publish must run only after the caller's transaction commits.
func (s *service) StoreFinalizationCandidateTx(ctx context.Context, tx *sql.Tx, m Message, id FinalizationIdentity) (Message, string, func(), error) {
	if !id.valid() || m.Role != Assistant {
		return Message{}, "", nil, ErrFinalizationCandidateChanged
	}
	if finish := m.FinishPart(); finish == nil || !finish.Partial || finish.FinalizationIdentity() != id {
		return Message{}, "", nil, ErrFinalizationCandidateChanged
	}
	q := db.New(tx)
	rows, partial, err := s.updateWith(ctx, q, m)
	if err != nil {
		return Message{}, "", nil, err
	}
	if !partial || rows != 1 {
		return Message{}, "", nil, ErrFinalizationCandidateChanged
	}
	row, err := q.GetMessage(ctx, m.ID)
	if err != nil {
		return Message{}, "", nil, err
	}
	stored, err := s.fromDBItem(row)
	if err != nil {
		return Message{}, "", nil, err
	}
	hash, err := stored.FinalizationContentHash()
	if err != nil {
		return Message{}, "", nil, err
	}
	if err := stored.VerifyFinalizationCandidate(id, hash); err != nil {
		return Message{}, "", nil, err
	}
	return stored, hash, func() { s.publishUpdate(ctx, stored, 1, true) }, nil
}

var (
	_ FinalizationReader = (*service)(nil)
	_ FinalizationWriter = (*service)(nil)
)
