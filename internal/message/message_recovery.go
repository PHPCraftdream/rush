package message

import (
	"context"
	"database/sql"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

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

// StampInterruptedAssistantIfStillLast implementation: thin wrapper around
// the sqlc-generated conditional query. See the Service interface doc
// comment and the query's own doc comment (internal/db/sql/messages.sql) for
// the full rationale.
//
// msg is expected to already carry the process-restart Finish part (the
// caller calls AddFinish before this) -- this only marshals and writes it,
// conditioned on the row still being both unfinished and the session's last
// message as of this single statement.
func (s *service) StampInterruptedAssistantIfStillLast(ctx context.Context, sessionID string, msg Message) (bool, error) {
	parts, err := marshalParts(msg.Parts)
	if err != nil {
		return false, err
	}
	finishedAt := sql.NullInt64{}
	if finish := msg.FinishPart(); finish != nil && !finish.Partial {
		finishedAt.Int64 = finish.Time
		finishedAt.Valid = true
	}
	rowsAffected, err := s.q.StampInterruptedAssistantIfStillLast(ctx, db.StampInterruptedAssistantIfStillLastParams{
		Parts:      string(parts),
		FinishedAt: finishedAt,
		ID:         msg.ID,
		SessionID:  sessionID,
	})
	if err != nil {
		return false, err
	}
	if rowsAffected == 0 {
		// Candidate went stale between discovery and this write: either
		// superseded by a newer message, or finished concurrently by its
		// live owner. Not an error -- the caller skips and does not retry.
		return false, nil
	}
	// Terminal write: PublishMustDeliver, matching Update's terminal-write
	// delivery semantics (a momentarily full subscriber buffer must not
	// silently eat this state).
	msg.UpdatedAt = time.Now().Unix()
	s.PublishMustDeliver(ctx, pubsub.UpdatedEvent, msg.Clone())
	return true, nil
}
