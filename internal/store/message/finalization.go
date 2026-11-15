package message

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
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
