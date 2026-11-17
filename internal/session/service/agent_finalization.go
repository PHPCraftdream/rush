package session

import (
	"context"
	"database/sql"
	"errors"
)

var (
	ErrAgentFinalizationChanged  = errors.New("finalization_scope_changed")
	ErrAgentFinalizationAccepted = errors.New("finalization_already_accepted")
)

// AgentFinalizationIdentity is trusted execution identity, never model input.
type AgentFinalizationIdentity struct {
	SessionID, LogicalCallID string
	Generation               int64
	NodeID                   string
	NodeToken                int64
}

func (id AgentFinalizationIdentity) valid() bool {
	return id.SessionID != "" && id.LogicalCallID != "" && id.Generation > 0 && id.NodeToken >= 0 && ((id.NodeID == "") == (id.NodeToken == 0))
}

type AgentFinalizationRecord struct {
	AgentFinalizationIdentity
	Revision                          int64
	Phase                             string
	CandidateMessageID, CandidateHash string
	// CandidateSerial is a high-water mark: it survives clearing a candidate so
	// an older provider step can never be recorded after a newer one.
	CandidateSerial int64
	OpenTickets     int64
	Cancelled       bool
}

func (r AgentFinalizationRecord) active() bool {
	return r.Phase == "executing" || r.Phase == "waiting_work" || r.Phase == "composing"
}

// AgentFinalizationStore fences productive writes against accepted generations.
// Every producer still has to acquire its ticket before side effects.
type AgentFinalizationStore struct{ conn *sql.DB }

func NewAgentFinalizationStore(conn *sql.DB) *AgentFinalizationStore {
	return &AgentFinalizationStore{conn: conn}
}

func (s *AgentFinalizationStore) Bind(ctx context.Context, id AgentFinalizationIdentity) error {
	if !id.valid() {
		return ErrAgentFinalizationChanged
	}
	_, err := s.conn.ExecContext(ctx, `INSERT INTO session_agent_finalizations
		(session_id, logical_call_id, generation, node_id, node_token) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(session_id, logical_call_id, generation) DO NOTHING`,
		id.SessionID, id.LogicalCallID, id.Generation, id.NodeID, id.NodeToken)
	if err != nil {
		return err
	}
	_, err = s.Snapshot(ctx, id)
	return err
}

type finalizationQueryRow interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readAgentFinalization(ctx context.Context, query finalizationQueryRow, id AgentFinalizationIdentity) (AgentFinalizationRecord, error) {
	if !id.valid() {
		return AgentFinalizationRecord{}, ErrAgentFinalizationChanged
	}
	var r AgentFinalizationRecord
	r.AgentFinalizationIdentity = id
	var node string
	var token int64
	err := query.QueryRowContext(ctx, `SELECT f.revision, f.phase, f.node_id, f.node_token,
		f.candidate_message_id, f.candidate_hash, f.candidate_serial, s.cancel_requested,
		(SELECT count(*) FROM session_agent_finalization_tickets t
		 WHERE t.session_id=f.session_id AND t.logical_call_id=f.logical_call_id
		 AND t.generation=f.generation AND t.released=0)
		FROM session_agent_finalizations f JOIN sessions s ON s.id=f.session_id
		WHERE f.session_id=? AND f.logical_call_id=? AND f.generation=?`,
		id.SessionID, id.LogicalCallID, id.Generation).Scan(
		&r.Revision, &r.Phase, &node, &token, &r.CandidateMessageID, &r.CandidateHash, &r.CandidateSerial, &r.Cancelled, &r.OpenTickets)
	if err != nil {
		return AgentFinalizationRecord{}, err
	}
	if node != id.NodeID || token != id.NodeToken {
		return AgentFinalizationRecord{}, ErrAgentFinalizationChanged
	}
	return r, nil
}

func (s *AgentFinalizationStore) Snapshot(ctx context.Context, id AgentFinalizationIdentity) (AgentFinalizationRecord, error) {
	return readAgentFinalization(ctx, s.conn, id)
}

// Admit persists a hold and revision before the producer may perform any work.
// Retrying an open ticket is idempotent; a released ticket cannot be reopened.
func (s *AgentFinalizationStore) Admit(ctx context.Context, id AgentFinalizationIdentity, ticket string) (AgentFinalizationRecord, error) {
	if ticket == "" {
		return AgentFinalizationRecord{}, ErrAgentFinalizationChanged
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return AgentFinalizationRecord{}, err
	}
	defer tx.Rollback()
	r, err := readAgentFinalization(ctx, tx, id)
	if err != nil {
		return AgentFinalizationRecord{}, err
	}
	if r.Cancelled {
		return AgentFinalizationRecord{}, context.Canceled
	}
	if !r.active() {
		return AgentFinalizationRecord{}, ErrAgentFinalizationChanged
	}
	var released int
	err = tx.QueryRowContext(ctx, `SELECT released FROM session_agent_finalization_tickets
		WHERE session_id=? AND logical_call_id=? AND generation=? AND ticket_id=?`,
		id.SessionID, id.LogicalCallID, id.Generation, ticket).Scan(&released)
	if err == nil {
		if released != 0 {
			return AgentFinalizationRecord{}, ErrAgentFinalizationChanged
		}
		return r, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AgentFinalizationRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO session_agent_finalization_tickets
		(session_id, logical_call_id, generation, ticket_id) VALUES (?, ?, ?, ?)`,
		id.SessionID, id.LogicalCallID, id.Generation, ticket)
	if err != nil {
		return AgentFinalizationRecord{}, err
	}
	if err := advanceAgentFinalization(ctx, tx, id); err != nil {
		return AgentFinalizationRecord{}, err
	}
	r.Revision++
	r.OpenTickets++
	return r, tx.Commit()
}

func advanceAgentFinalization(ctx context.Context, tx *sql.Tx, id AgentFinalizationIdentity) error {
	result, err := tx.ExecContext(ctx, `UPDATE session_agent_finalizations SET revision=revision+1
		WHERE session_id=? AND logical_call_id=? AND generation=? AND revision<9223372036854775807`,
		id.SessionID, id.LogicalCallID, id.Generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrAgentFinalizationChanged
	}
	return nil
}

// Release keeps a durable receipt so duplicate completion cannot lower the hold twice.
func (s *AgentFinalizationStore) Release(ctx context.Context, id AgentFinalizationIdentity, ticket string) error {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := readAgentFinalization(ctx, tx, id); err != nil {
		return err
	}
	var released int
	if err := tx.QueryRowContext(ctx, `SELECT released FROM session_agent_finalization_tickets
		WHERE session_id=? AND logical_call_id=? AND generation=? AND ticket_id=?`,
		id.SessionID, id.LogicalCallID, id.Generation, ticket).Scan(&released); err != nil {
		return err
	}
	if released != 0 {
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `UPDATE session_agent_finalization_tickets SET released=1
		WHERE session_id=? AND logical_call_id=? AND generation=? AND ticket_id=? AND released=0`,
		id.SessionID, id.LogicalCallID, id.Generation, ticket)
	if err != nil {
		return err
	}
	if err := advanceAgentFinalization(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SealTx shares the candidate publication's writer transaction. It does not read
// every owned-work source: the caller must also validate complete facts in tx.
// It seals only the candidate previously recorded in the record (same message,
// stored-parts hash and provider-step serial) at the same revision.
func (s *AgentFinalizationStore) SealTx(ctx context.Context, tx *sql.Tx, id AgentFinalizationIdentity, c AgentFinalizationCandidate) error {
	if !c.valid() {
		return ErrAgentFinalizationChanged
	}
	r, err := readAgentFinalization(ctx, tx, id)
	if err != nil {
		return err
	}
	if r.Cancelled {
		return context.Canceled
	}
	matches := r.CandidateMessageID == c.MessageID && r.CandidateHash == c.Hash && r.CandidateSerial == c.Serial
	if r.Phase == "accepted" && r.Revision == c.Revision && matches {
		return ErrAgentFinalizationAccepted
	}
	if !r.active() || r.Revision != c.Revision || r.OpenTickets != 0 || !matches {
		return ErrAgentFinalizationChanged
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_agent_finalizations
		SET phase='accepted'
		WHERE session_id=? AND logical_call_id=? AND generation=? AND revision=?
		AND candidate_message_id=? AND candidate_hash=? AND candidate_serial=?
		AND phase IN ('executing', 'waiting_work', 'composing')
		AND NOT EXISTS (SELECT 1 FROM session_agent_finalization_tickets t
		 WHERE t.session_id=session_agent_finalizations.session_id
		 AND t.logical_call_id=session_agent_finalizations.logical_call_id
		 AND t.generation=session_agent_finalizations.generation AND t.released=0)
		AND EXISTS (SELECT 1 FROM sessions s WHERE s.id=session_agent_finalizations.session_id AND s.cancel_requested=0)`,
		id.SessionID, id.LogicalCallID, id.Generation, c.Revision, c.MessageID, c.Hash, c.Serial)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrAgentFinalizationChanged
	}
	return nil
}
