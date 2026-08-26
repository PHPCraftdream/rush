package message

import "context"

// InterruptedAssistantCandidate is one row of
// ListCandidateInterruptedAssistantSessions: a session whose chronologically
// last message is an unfinished assistant message, i.e. a candidate for
// startup recovery (task #774).
type InterruptedAssistantCandidate struct {
	SessionID string
	MessageID string
	// ParentSessionID is empty for a top-level session, matching
	// session.Session.ParentSessionID's convention.
	ParentSessionID string
}

// ListCandidateInterruptedAssistantSessions implementation: thin wrapper
// around the sqlc-generated query. See the Service interface doc comment and
// the query's own doc comment (internal/db/sql/messages.sql) for the full
// rationale and correctness notes.
func (s *service) ListCandidateInterruptedAssistantSessions(ctx context.Context) ([]InterruptedAssistantCandidate, error) {
	rows, err := s.qRead.ListCandidateInterruptedAssistantSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]InterruptedAssistantCandidate, len(rows))
	for i, row := range rows {
		out[i] = InterruptedAssistantCandidate{
			SessionID:       row.SessionID,
			MessageID:       row.MessageID,
			ParentSessionID: row.ParentSessionID.String,
		}
	}
	return out, nil
}
