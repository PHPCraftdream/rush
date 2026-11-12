package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/message"
)

// AgentFinalizationSealReason is the typed cause of a TrySeal refusal. The
// zero value is not a refusal; every other value keeps its snake_case code
// stable so callers can branch on it.
type AgentFinalizationSealReason uint8

const (
	AgentFinalizationSealNone      AgentFinalizationSealReason = iota // not refused
	AgentFinalizationSealCancelled                                    // "cancel_requested"
	// AgentFinalizationSealWrongPhase: the record failed, was cancelled or was
	// interrupted, so this incarnation can no longer seal anything.
	AgentFinalizationSealWrongPhase // "wrong_phase"
	// AgentFinalizationSealNoCandidate: nothing was ever recorded to seal.
	AgentFinalizationSealNoCandidate // "candidate_not_recorded"
	// AgentFinalizationSealStaleCandidate: the presented message, hash or
	// serial is not the recorded candidate (or an accepted record disagrees).
	AgentFinalizationSealStaleCandidate // "candidate_stale"
	// AgentFinalizationSealRevisionChanged: the record moved since the
	// candidate was composed.
	AgentFinalizationSealRevisionChanged // "revision_changed"
	// AgentFinalizationSealOpenTickets: a mutation ticket is still open.
	AgentFinalizationSealOpenTickets // "open_mutation_tickets"
	// AgentFinalizationSealScopeIncomplete: the scope facts were missing or
	// not established; the seal refuses (fail-closed default).
	AgentFinalizationSealScopeIncomplete // "scope_facts_unestablished"
	// AgentFinalizationSealDescendantsActive: owned child work is still live.
	AgentFinalizationSealDescendantsActive // "descendants_active"
	// AgentFinalizationSealReactionDebt: notification reactions are pending.
	AgentFinalizationSealReactionDebt // "reaction_debt_open"
	// AgentFinalizationSealPhysicalHolds: durable physical holds are open.
	AgentFinalizationSealPhysicalHolds // "physical_holds_open"
	// AgentFinalizationSealQueuedCalls: queued owned calls still exist.
	AgentFinalizationSealQueuedCalls // "queued_calls_present"
	// AgentFinalizationSealCandidateDamaged: the stored row no longer
	// verifies against the recorded hash or identity.
	AgentFinalizationSealCandidateDamaged // "candidate_damaged"
)

func (r AgentFinalizationSealReason) String() string {
	switch r {
	case AgentFinalizationSealNone:
		return "none"
	case AgentFinalizationSealCancelled:
		return "cancel_requested"
	case AgentFinalizationSealWrongPhase:
		return "wrong_phase"
	case AgentFinalizationSealNoCandidate:
		return "candidate_not_recorded"
	case AgentFinalizationSealStaleCandidate:
		return "candidate_stale"
	case AgentFinalizationSealRevisionChanged:
		return "revision_changed"
	case AgentFinalizationSealOpenTickets:
		return "open_mutation_tickets"
	case AgentFinalizationSealScopeIncomplete:
		return "scope_facts_unestablished"
	case AgentFinalizationSealDescendantsActive:
		return "descendants_active"
	case AgentFinalizationSealReactionDebt:
		return "reaction_debt_open"
	case AgentFinalizationSealPhysicalHolds:
		return "physical_holds_open"
	case AgentFinalizationSealQueuedCalls:
		return "queued_calls_present"
	case AgentFinalizationSealCandidateDamaged:
		return "candidate_damaged"
	default:
		return fmt.Sprintf("seal_reason_%d", uint8(r))
	}
}

// AgentFinalizationSealOutcome is the deterministic result of one TrySeal.
// Sealed and Accepted are mutually exclusive successes; every refusal keeps
// the reason and the record snapshot read when the decision was made.
type AgentFinalizationSealOutcome struct {
	Sealed   bool
	Accepted bool // idempotent re-seal of an already accepted record, zero writes
	Reason   AgentFinalizationSealReason
	Record   AgentFinalizationRecord
	Message  message.Message // the sealed/accepted row; zero when refused
}

// AgentFinalizationScopeFacts carries the owned-scope facts this layer cannot
// produce yet (descendants, reaction debt, physical holds, queued calls).
// Complete must assert every field was actually observed inside the seal's
// transaction; the zero value and any false Complete refuse.
type AgentFinalizationScopeFacts struct {
	Complete          bool
	ActiveDescendants int
	ReactionDebt      int
	PhysicalHolds     int
	QueuedCalls       int
}

// AgentFinalizationScopeReader reads scope facts inside TrySeal's writer
// transaction. A nil reader is the S3 default and always refuses.
type AgentFinalizationScopeReader interface {
	FinalizationSealScopeFacts(ctx context.Context, tx *sql.Tx) (AgentFinalizationScopeFacts, error)
}

// FinalizationSealWriter reads the stored candidate and persists its accepted
// receipt inside TrySeal's transaction. *message.Service satisfies it.
type FinalizationSealWriter interface {
	message.FinalizationReader
	UpdateTx(ctx context.Context, tx *sql.Tx, m message.Message) (func(), error)
}

// TrySeal seals the previously recorded candidate when every condition known
// at this layer holds, in one transaction. The checks run in a fixed order:
// context, argument validity, persisted cancel, already-accepted idempotence,
// record phase, recorded-candidate presence and identity, open tickets,
// revision, scope facts (fail-closed), in-transaction candidate verification,
// then the record seal and the accepted receipt write. Refusals roll back
// without writing. The scope reader is fail-closed: nil or unestablished
// facts never seal.
func (s *AgentFinalizationStore) TrySeal(ctx context.Context, id AgentFinalizationIdentity, c AgentFinalizationCandidate, messages FinalizationSealWriter, scope AgentFinalizationScopeReader) (AgentFinalizationSealOutcome, error) {
	if ctx.Err() != nil {
		return AgentFinalizationSealOutcome{}, ctx.Err()
	}
	if !id.valid() || !c.valid() || messages == nil {
		return AgentFinalizationSealOutcome{}, ErrAgentFinalizationChanged
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return AgentFinalizationSealOutcome{}, err
	}
	defer tx.Rollback()
	r, err := readAgentFinalization(ctx, tx, id)
	if err != nil {
		return AgentFinalizationSealOutcome{}, err
	}
	if r.Cancelled {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealCancelled, Record: r}, nil
	}
	if r.Phase == "accepted" {
		matches := r.Revision == c.Revision && r.CandidateMessageID == c.MessageID && r.CandidateHash == c.Hash && r.CandidateSerial == c.Serial
		if !matches {
			return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealStaleCandidate, Record: r}, nil
		}
		stored, err := messages.GetFinalizationCandidateTx(ctx, tx, c.MessageID)
		if errors.Is(err, sql.ErrNoRows) {
			return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealCandidateDamaged, Record: r}, nil
		}
		if err != nil {
			return AgentFinalizationSealOutcome{}, err
		}
		finish := stored.FinishPart()
		accepted := stored.SessionID == id.SessionID && finish != nil && !finish.Partial &&
			finish.Reason == message.FinishReasonEndTurn && finish.FinalizationHash == c.Hash &&
			finish.FinalizationIdentity() == c.identity(id)
		if !accepted {
			return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealCandidateDamaged, Record: r}, nil
		}
		return AgentFinalizationSealOutcome{Accepted: true, Record: r, Message: stored}, nil
	}
	if !r.active() {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealWrongPhase, Record: r}, nil
	}
	if r.CandidateMessageID == "" && r.CandidateHash == "" {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealNoCandidate, Record: r}, nil
	}
	recorded := AgentFinalizationCandidate{MessageID: r.CandidateMessageID, Hash: r.CandidateHash, Revision: r.Revision, Serial: r.CandidateSerial}
	if r.CandidateMessageID == "" || r.CandidateHash == "" || !recorded.valid() {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealCandidateDamaged, Record: r}, nil
	}
	if r.CandidateMessageID != c.MessageID || r.CandidateHash != c.Hash || r.CandidateSerial != c.Serial {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealStaleCandidate, Record: r}, nil
	}
	if r.OpenTickets > 0 {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealOpenTickets, Record: r}, nil
	}
	if r.Revision != c.Revision {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealRevisionChanged, Record: r}, nil
	}
	if scope == nil {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealScopeIncomplete, Record: r}, nil
	}
	facts, err := scope.FinalizationSealScopeFacts(ctx, tx)
	if err != nil {
		return AgentFinalizationSealOutcome{}, fmt.Errorf("read finalization scope facts: %w", err)
	}
	if !facts.Complete {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealScopeIncomplete, Record: r}, nil
	}
	switch {
	case facts.ActiveDescendants > 0:
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealDescendantsActive, Record: r}, nil
	case facts.ReactionDebt > 0:
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealReactionDebt, Record: r}, nil
	case facts.PhysicalHolds > 0:
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealPhysicalHolds, Record: r}, nil
	case facts.QueuedCalls > 0:
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealQueuedCalls, Record: r}, nil
	}
	stored, err := messages.GetFinalizationCandidateTx(ctx, tx, c.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealCandidateDamaged, Record: r}, nil
	}
	if err != nil {
		return AgentFinalizationSealOutcome{}, err
	}
	if stored.SessionID != id.SessionID || stored.VerifyFinalizationCandidate(c.identity(id), c.Hash) != nil {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealCandidateDamaged, Record: r}, nil
	}
	acceptedRow := stored
	if err := acceptedRow.AcceptFinalizationCandidate(c.identity(id), c.Hash); err != nil {
		return AgentFinalizationSealOutcome{}, err
	}
	sealErr := s.SealTx(ctx, tx, id, c)
	if errors.Is(sealErr, context.Canceled) {
		return AgentFinalizationSealOutcome{Reason: AgentFinalizationSealCancelled, Record: r}, nil
	}
	if sealErr != nil {
		return AgentFinalizationSealOutcome{}, sealErr
	}
	publish, err := messages.UpdateTx(ctx, tx, acceptedRow)
	if err != nil {
		return AgentFinalizationSealOutcome{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentFinalizationSealOutcome{}, err
	}
	r.Phase = "accepted"
	if publish != nil {
		publish()
	}
	return AgentFinalizationSealOutcome{Sealed: true, Record: r, Message: acceptedRow}, nil
}
