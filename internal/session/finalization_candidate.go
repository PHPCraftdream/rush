package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/PHPCraftdream/rush/internal/message"
)

// ErrAgentFinalizationCandidateStale marks a candidate writer whose revision or
// provider-step serial is no longer current. It is a kind of scope change.
var ErrAgentFinalizationCandidateStale = fmt.Errorf("%w: stale candidate", ErrAgentFinalizationChanged)

// AgentFinalizationCandidate is the durable identity of a provisional message:
// the message row, the hash of its stored parts, the record revision it was
// composed against and the provider-step serial that produced it. The logical
// call and generation are the owning record's own key.
type AgentFinalizationCandidate struct {
	MessageID, Hash  string
	Revision, Serial int64
}

func (c AgentFinalizationCandidate) valid() bool {
	return c.MessageID != "" && c.Hash != "" && c.Revision >= 0 && c.Serial > 0
}

// CandidateOf converts a message-level step identity and a stored-parts hash.
func CandidateOf(messageID, hash string, id message.FinalizationIdentity) (AgentFinalizationCandidate, bool) {
	if id.Revision > math.MaxInt64 || id.Serial > math.MaxInt64 {
		return AgentFinalizationCandidate{}, false
	}
	c := AgentFinalizationCandidate{MessageID: messageID, Hash: hash, Revision: int64(id.Revision), Serial: int64(id.Serial)}
	return c, c.valid()
}

func (c AgentFinalizationCandidate) identity(id AgentFinalizationIdentity) message.FinalizationIdentity {
	return message.FinalizationIdentity{Key: id.LogicalCallID, Generation: uint64(id.Generation), Revision: uint64(c.Revision), Serial: uint64(c.Serial)}
}

// fenceCandidate is the write-side fence shared by the pre-write check and the
// conditional UPDATE: the owner must be live and active, composed against the
// current revision, and the step must be newer than every recorded one.
func fenceCandidate(r AgentFinalizationRecord, revision, serial int64) error {
	if r.Cancelled {
		return context.Canceled
	}
	if !r.active() {
		return ErrAgentFinalizationChanged
	}
	if r.Revision != revision || serial <= r.CandidateSerial {
		return ErrAgentFinalizationCandidateStale
	}
	return nil
}

// StoreCandidateTx writes the candidate message and records its identity in one
// writer transaction, so no process can observe the row without the record or
// the reverse. Both writes sit behind the record's revision and the message's
// checkpoint-generation fence; any refusal leaves both untouched once the
// caller rolls tx back. publish must run only after tx commits.
func (s *AgentFinalizationStore) StoreCandidateTx(ctx context.Context, tx *sql.Tx, id AgentFinalizationIdentity, messages message.FinalizationWriter, m message.Message, step message.FinalizationIdentity) (message.Message, func(), error) {
	if messages == nil || step.Key != id.LogicalCallID || id.Generation <= 0 || step.Generation != uint64(id.Generation) || m.SessionID != id.SessionID {
		return message.Message{}, nil, ErrAgentFinalizationChanged
	}
	probe, ok := CandidateOf(m.ID, "-", step)
	if !ok {
		return message.Message{}, nil, ErrAgentFinalizationChanged
	}
	r, err := readAgentFinalization(ctx, tx, id)
	if err != nil {
		return message.Message{}, nil, err
	}
	if err := fenceCandidate(r, probe.Revision, probe.Serial); err != nil {
		return message.Message{}, nil, err
	}
	stored, hash, publish, err := messages.StoreFinalizationCandidateTx(ctx, tx, m, step)
	if err != nil {
		return message.Message{}, nil, err
	}
	if stored.SessionID != id.SessionID || stored.ID != m.ID {
		return message.Message{}, nil, message.ErrFinalizationCandidateChanged
	}
	probe.Hash = hash
	result, err := tx.ExecContext(ctx, `UPDATE session_agent_finalizations
		SET candidate_message_id=?, candidate_hash=?, candidate_serial=?
		WHERE session_id=? AND logical_call_id=? AND generation=? AND revision=?
		AND phase IN ('executing', 'waiting_work', 'composing') AND candidate_serial<?
		AND EXISTS (SELECT 1 FROM sessions s WHERE s.id=session_agent_finalizations.session_id AND s.cancel_requested=0)`,
		probe.MessageID, probe.Hash, probe.Serial,
		id.SessionID, id.LogicalCallID, id.Generation, probe.Revision, probe.Serial)
	if err != nil {
		return message.Message{}, nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return message.Message{}, nil, err
	}
	if changed != 1 {
		return message.Message{}, nil, ErrAgentFinalizationCandidateStale
	}
	return stored, publish, nil
}

// AgentFinalizationRecovery is the deterministic outcome for a persisted candidate.
type AgentFinalizationRecovery uint8

const (
	// AgentFinalizationNoCandidate: nothing was recorded; the owner composes afresh.
	AgentFinalizationNoCandidate AgentFinalizationRecovery = iota
	// AgentFinalizationCandidateAdopted: the recorded candidate is intact and
	// still current, so the owner may seal it.
	AgentFinalizationCandidateAdopted
	// AgentFinalizationCandidateFailed: the recorded candidate was invalid or
	// outdated and has been cleared; the owner composes afresh.
	AgentFinalizationCandidateFailed
)

// RecoverCandidate re-adopts or fails the candidate recorded for id, in one
// transaction. A candidate is adopted only when its message still exists in the
// owner session as an unfinished candidate carrying this call, generation,
// recorded serial and the current record revision, and its stored parts still
// hash to the recorded hash. Anything else clears the candidate but keeps the
// serial high-water mark. Callers resume their step counter above
// Record.CandidateSerial for every outcome.
func (s *AgentFinalizationStore) RecoverCandidate(ctx context.Context, id AgentFinalizationIdentity, messages message.FinalizationReader) (AgentFinalizationRecovery, AgentFinalizationRecord, error) {
	if messages == nil {
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, ErrAgentFinalizationChanged
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, err
	}
	defer tx.Rollback()
	r, err := readAgentFinalization(ctx, tx, id)
	if err != nil {
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, err
	}
	if r.Cancelled {
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, context.Canceled
	}
	if !r.active() {
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, ErrAgentFinalizationChanged
	}
	if r.CandidateMessageID == "" && r.CandidateHash == "" {
		return AgentFinalizationNoCandidate, r, tx.Commit()
	}
	intact, err := candidateIntact(ctx, tx, messages, id, r)
	if err != nil {
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, err
	}
	if intact {
		return AgentFinalizationCandidateAdopted, r, tx.Commit()
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_agent_finalizations
		SET candidate_message_id='', candidate_hash=''
		WHERE session_id=? AND logical_call_id=? AND generation=? AND revision=? AND candidate_serial=?
		AND candidate_message_id=? AND candidate_hash=? AND phase IN ('executing', 'waiting_work', 'composing')`,
		id.SessionID, id.LogicalCallID, id.Generation, r.Revision, r.CandidateSerial, r.CandidateMessageID, r.CandidateHash)
	if err != nil {
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err == nil {
			err = ErrAgentFinalizationChanged
		}
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentFinalizationNoCandidate, AgentFinalizationRecord{}, err
	}
	r.CandidateMessageID, r.CandidateHash = "", ""
	return AgentFinalizationCandidateFailed, r, nil
}

// candidateIntact treats a missing row as a failed candidate; any other read
// error is returned so a transient fault never clears a good candidate.
func candidateIntact(ctx context.Context, tx *sql.Tx, messages message.FinalizationReader, id AgentFinalizationIdentity, r AgentFinalizationRecord) (bool, error) {
	c := AgentFinalizationCandidate{MessageID: r.CandidateMessageID, Hash: r.CandidateHash, Revision: r.Revision, Serial: r.CandidateSerial}
	if !c.valid() {
		return false, nil
	}
	stored, err := messages.GetFinalizationCandidateTx(ctx, tx, c.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return stored.SessionID == id.SessionID && stored.VerifyFinalizationCandidate(c.identity(id), c.Hash) == nil, nil
}
