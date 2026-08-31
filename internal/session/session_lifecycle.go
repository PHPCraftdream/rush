// Session lifecycle: the Create*/Delete row-management methods, plus the
// agent-tool sub-session ID scheme ("messageID$$toolCallID") that gives
// every agent tool call its own durable session row.

package session

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/google/uuid"
)

func (s *service) createWithOrigin(ctx context.Context, id string, title string, origin message.Origin) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:     id,
		Title:  title,
		Origin: string(origin),
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) Create(ctx context.Context, title string) (Session, error) {
	return s.createWithOrigin(ctx, uuid.New().String(), title, message.OriginUnspecified)
}

func (s *service) CreateWithID(ctx context.Context, id, title string) (Session, error) {
	return s.createWithOrigin(ctx, id, title, message.OriginUnspecified)
}

func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              toolCallID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           title,
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		ID:              "title-" + parentSessionID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           "Generate a title",
	})
	if err != nil {
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

func (s *service) Delete(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	qtx := s.q.WithTx(tx)

	dbSession, err := qtx.GetSessionByID(ctx, id)
	if err != nil {
		return err
	}
	if err = qtx.DeleteSessionMessages(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session messages: %w", err)
	}
	if err = qtx.DeleteSessionFiles(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session files: %w", err)
	}
	if err = qtx.DeleteSession(ctx, dbSession.ID); err != nil {
		return fmt.Errorf("deleting session: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}

	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.DeletedEvent, session)
	return nil
}

// CreateAgentToolSessionID creates a session ID for agent tool sessions using the format "messageID$$toolCallID"
func (s *service) CreateAgentToolSessionID(messageID, toolCallID string) string {
	return fmt.Sprintf("%s$$%s", messageID, toolCallID)
}

// ParseAgentToolSessionID parses an agent tool session ID into its components
func (s *service) ParseAgentToolSessionID(sessionID string) (messageID string, toolCallID string, ok bool) {
	parts := strings.Split(sessionID, "$$")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsAgentToolSession checks if a session ID follows the agent tool session format
func (s *service) IsAgentToolSession(sessionID string) bool {
	_, _, ok := s.ParseAgentToolSessionID(sessionID)
	return ok
}

// CreateWithOrigin is Create plus an explicit entry-channel origin
// (message.OriginCLI/Web/SDK) persisted on the session row.
func (s *service) CreateWithOrigin(ctx context.Context, title string, origin message.Origin) (Session, error) {
	return s.createWithOrigin(ctx, uuid.New().String(), title, origin)
}

// CreateWithIDAndOrigin is CreateWithID plus an explicit entry-channel
// origin persisted on the session row.
func (s *service) CreateWithIDAndOrigin(ctx context.Context, id, title string, origin message.Origin) (Session, error) {
	return s.createWithOrigin(ctx, id, title, origin)
}

// OriginCreator is the consuming-package seam for the origin-aware
// Create siblings above. It is deliberately NOT part of Service: adding
// methods to Service would force every test fake implementing it across
// the repo to grow stubs. Callers (internal/app, internal/server)
// type-assert their session.Service value to OriginCreator and fall back
// to the plain Create/CreateWithID when the assertion fails — the same
// pattern internal/agent's credentialRunner uses for
// Coordinator.RunWithCredentials.
type OriginCreator interface {
	CreateWithOrigin(ctx context.Context, title string, origin message.Origin) (Session, error)
	CreateWithIDAndOrigin(ctx context.Context, id, title string, origin message.Origin) (Session, error)
}

var _ OriginCreator = (*service)(nil)
