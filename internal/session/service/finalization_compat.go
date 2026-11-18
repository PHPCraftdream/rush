package session

import (
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/session/batch/finterm"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

type (
	AgentFinalizationIdentity  = finterm.AgentFinalizationIdentity
	AgentFinalizationRecord    = finterm.AgentFinalizationRecord
	AgentFinalizationCandidate = finterm.AgentFinalizationCandidate
	AgentFinalizationStore     = finterm.AgentFinalizationStore

	AgentFinalizationRecovery = finterm.AgentFinalizationRecovery

	AgentFinalizationSealReason  = finterm.AgentFinalizationSealReason
	AgentFinalizationSealOutcome = finterm.AgentFinalizationSealOutcome
	AgentFinalizationScopeFacts  = finterm.AgentFinalizationScopeFacts
	AgentFinalizationScopeReader = finterm.AgentFinalizationScopeReader
	FinalizationSealWriter       = finterm.FinalizationSealWriter
)

const (
	AgentFinalizationSealNone              = finterm.AgentFinalizationSealNone
	AgentFinalizationSealCancelled         = finterm.AgentFinalizationSealCancelled
	AgentFinalizationSealWrongPhase        = finterm.AgentFinalizationSealWrongPhase
	AgentFinalizationSealNoCandidate       = finterm.AgentFinalizationSealNoCandidate
	AgentFinalizationSealStaleCandidate    = finterm.AgentFinalizationSealStaleCandidate
	AgentFinalizationSealRevisionChanged   = finterm.AgentFinalizationSealRevisionChanged
	AgentFinalizationSealOpenTickets       = finterm.AgentFinalizationSealOpenTickets
	AgentFinalizationSealScopeIncomplete   = finterm.AgentFinalizationSealScopeIncomplete
	AgentFinalizationSealDescendantsActive = finterm.AgentFinalizationSealDescendantsActive
	AgentFinalizationSealReactionDebt      = finterm.AgentFinalizationSealReactionDebt
	AgentFinalizationSealPhysicalHolds     = finterm.AgentFinalizationSealPhysicalHolds
	AgentFinalizationSealQueuedCalls       = finterm.AgentFinalizationSealQueuedCalls
	AgentFinalizationSealCandidateDamaged  = finterm.AgentFinalizationSealCandidateDamaged
)

const (
	AgentFinalizationNoCandidate      = finterm.AgentFinalizationNoCandidate
	AgentFinalizationCandidateAdopted = finterm.AgentFinalizationCandidateAdopted
	AgentFinalizationCandidateFailed  = finterm.AgentFinalizationCandidateFailed
)

var (
	ErrAgentFinalizationChanged        = finterm.ErrAgentFinalizationChanged
	ErrAgentFinalizationAccepted       = finterm.ErrAgentFinalizationAccepted
	ErrAgentFinalizationCandidateStale = finterm.ErrAgentFinalizationCandidateStale
)

func NewAgentFinalizationStore(conn *sql.DB) *AgentFinalizationStore {
	return finterm.NewAgentFinalizationStore(conn)
}

func CandidateOf(messageID, hash string, id message.FinalizationIdentity) (AgentFinalizationCandidate, bool) {
	return finterm.CandidateOf(messageID, hash, id)
}
