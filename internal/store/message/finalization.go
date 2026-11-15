package message

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/PHPCraftdream/rush/internal/store/db"
)

var (
	ErrFinalizationCandidateChanged = errors.New("finalization_candidate_changed")
	ErrFinalizationAlreadyAccepted  = errors.New("finalization_already_accepted")
)

// FinalizationIdentity is backend-owned and is not an input capability.
type FinalizationIdentity struct {
	Key        string
	Generation uint64
	Revision   uint64
	Serial     uint64
}

func (id FinalizationIdentity) valid() bool {
	return id.Key != "" && id.Generation != 0 && id.Serial != 0
}

// FinalizationContentHash binds an immutable message snapshot, excluding its footer.
// Output-contract validation remains a separate check on the actual final text.
func (m Message) FinalizationContentHash() (string, error) {
	parts := make([]ContentPart, 0, len(m.Parts))
	for _, part := range m.Parts {
		if _, finish := part.(Finish); !finish {
			parts = append(parts, part)
		}
	}
	raw, err := marshalParts(parts)
	if err != nil {
		return "", fmt.Errorf("hash finalization candidate: %w", err)
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

// MarkFinalizationCandidate keeps a provider-ended message unfinished.
func (m *Message) MarkFinalizationCandidate(id FinalizationIdentity) error {
	if m.Role != Assistant || m.ID == "" || m.SessionID == "" || !id.valid() || m.IsFinished() {
		return ErrFinalizationCandidateChanged
	}
	parts := make([]ContentPart, 0, len(m.Parts)+1)
	for _, part := range m.Parts {
		if _, finish := part.(Finish); !finish {
			parts = append(parts, part)
		}
	}
	m.Parts = append(parts, Finish{
		Reason: FinishReasonUnknown, Time: time.Now().Unix(), Partial: true,
		FinalizationKey: id.Key, FinalizationGeneration: id.Generation,
		FinalizationRevision: id.Revision, FinalizationSerial: id.Serial,
	})
	return nil
}

// AcceptFinalizationCandidate must be persisted under the caller's atomic seal.
func (m *Message) AcceptFinalizationCandidate(id FinalizationIdentity, expectedHash string) error {
	if m.Role != Assistant || !id.valid() || expectedHash == "" {
		return ErrFinalizationCandidateChanged
	}
	for i, part := range m.Parts {
		finish, ok := part.(Finish)
		if !ok {
			continue
		}
		if finish.FinalizationKey != id.Key || finish.FinalizationGeneration != id.Generation || finish.FinalizationRevision != id.Revision || finish.FinalizationSerial != id.Serial {
			return ErrFinalizationCandidateChanged
		}
		hash, err := m.FinalizationContentHash()
		if err != nil {
			return err
		}
		if hash != expectedHash {
			return ErrFinalizationCandidateChanged
		}
		if !finish.Partial {
			if finish.Reason == FinishReasonEndTurn && finish.FinalizationHash == expectedHash {
				return ErrFinalizationAlreadyAccepted
			}
			return ErrFinalizationCandidateChanged
		}
		finish.Partial = false
		finish.Reason = FinishReasonEndTurn
		finish.Time = time.Now().Unix()
		finish.FinalizationHash = expectedHash
		m.Parts[i] = finish
		return nil
	}
	return ErrFinalizationCandidateChanged
}

// VerifyFinalizationCandidate reports whether m is still the provisional
// candidate (an unfinished Partial footer carrying exactly id) whose stored
// parts hash to expectedHash. It never mutates m.
func (m Message) VerifyFinalizationCandidate(id FinalizationIdentity, expectedHash string) error {
	if m.Role != Assistant || !id.valid() || expectedHash == "" {
		return ErrFinalizationCandidateChanged
	}
	finish := m.FinishPart()
	if finish == nil || !finish.Partial ||
		finish.FinalizationKey != id.Key || finish.FinalizationGeneration != id.Generation ||
		finish.FinalizationRevision != id.Revision || finish.FinalizationSerial != id.Serial {
		return ErrFinalizationCandidateChanged
	}
	hash, err := m.FinalizationContentHash()
	if err != nil {
		return err
	}
	if hash != expectedHash {
		return ErrFinalizationCandidateChanged
	}
	return nil
}

// FinalizationIdentity returns the identity carried by a candidate footer.
func (f Finish) FinalizationIdentity() FinalizationIdentity {
	return FinalizationIdentity{Key: f.FinalizationKey, Generation: f.FinalizationGeneration, Revision: f.FinalizationRevision, Serial: f.FinalizationSerial}
}

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
