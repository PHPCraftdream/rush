// Session lifecycle: the Create*/Delete row-management methods, plus the
// agent-tool sub-session ID scheme ("messageID$$toolCallID") that gives
// every agent tool call its own durable session row.

package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/google/uuid"
	sqlitedriver "modernc.org/sqlite"
)

// bindWorkspace is the ONE place a new session row is bound to the process
// that creates it (#1142 step C, invariant WS-1): workspace_root is the
// service's own workspace, git_branch is read from <git-dir>/HEAD at creation
// time. Every creation path (createWithOrigin, the task/title rows, and the
// fork in session_fork.go) fills its CreateSessionParams through here, so a
// session can never end up bound to some other process's workspace.
func (s *service) bindWorkspace(p db.CreateSessionParams) db.CreateSessionParams {
	p.WorkspaceRoot = s.workspaceRoot
	p.GitBranch = gitBranchFromWorkDir(s.workDir)
	return p
}

func (s *service) createWithOrigin(ctx context.Context, id string, title string, origin message.Origin) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, s.bindWorkspace(db.CreateSessionParams{
		ID:     id,
		Title:  title,
		Origin: string(origin),
	}))
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

// CreateTaskSession creates the durable per-delegation session row keyed by
// toolCallID (deterministic: async_jobs.child_session_id is claimed with the
// SAME id before this row exists, doc sec.3.8). Idempotent for a repeated
// call naming the SAME (toolCallID, parentSessionID) pair -- closes task
// #1038: a delegation whose claim already committed the row (e.g. a retried
// tool call, or an ASYNC-01 dead-host recovery that reused the id) must get
// the EXISTING session back, not a UNIQUE-constraint error. A toolCallID
// that already exists under a DIFFERENT parent stays an error -- id reuse
// across unrelated parents is never silently accepted.
func (s *service) CreateTaskSession(ctx context.Context, toolCallID, parentSessionID, title string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, s.bindWorkspace(db.CreateSessionParams{
		ID:              toolCallID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           title,
		// The delegation edge IS the cost-tree edge (#1130): set once at
		// creation, never re-pointed.
		CostParentID: parentSessionID,
	}))
	if err != nil {
		if isSessionsIDUniqueConstraintError(err) {
			existing, getErr := s.q.GetSessionByID(ctx, toolCallID)
			if getErr != nil {
				return Session{}, fmt.Errorf("create task session: id %s already exists but re-read failed: %w", toolCallID, getErr)
			}
			if !existing.ParentSessionID.Valid || existing.ParentSessionID.String != parentSessionID {
				return Session{}, fmt.Errorf("create task session: id %s already exists under a different parent", toolCallID)
			}
			return s.fromDBItem(existing), nil
		}
		return Session{}, err
	}
	session := s.fromDBItem(dbSession)
	s.Publish(pubsub.CreatedEvent, session)
	return session, nil
}

// isSessionsIDUniqueConstraintError reports whether err is a SQLite PRIMARY
// KEY/UNIQUE constraint violation on sessions.id -- the shape
// CreateTaskSession's idempotent-retry path (task #1038) needs to
// distinguish from any other insert failure. Same two-layer approach as
// internal/app's isSessionsIDConstraintError (typed Code()&0xff==19 fast
// path, plus a textual "constraint failed"+"sessions.id" check for drivers
// without a typed error) -- duplicated here rather than imported because
// internal/app depends on internal/session, not the other way around.
func isSessionsIDUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	const sqliteConstraintCode = 19
	var sqliteErr *sqlitedriver.Error
	isTypedConstraint := errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqliteConstraintCode
	msg := err.Error()
	isTextualConstraint := strings.Contains(msg, "constraint failed")
	if !isTypedConstraint && !isTextualConstraint {
		return false
	}
	return strings.Contains(msg, "sessions.id")
}

func (s *service) CreateTitleSession(ctx context.Context, parentSessionID string) (Session, error) {
	dbSession, err := s.q.CreateSession(ctx, s.bindWorkspace(db.CreateSessionParams{
		ID:              "title-" + parentSessionID,
		ParentSessionID: sql.NullString{String: parentSessionID, Valid: true},
		Title:           "Generate a title",
	}))
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
