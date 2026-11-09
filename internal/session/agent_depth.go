package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

const (
	maxAgentDepth     = 32
	defaultAgentDepth = 2
)

// AgentDepthStore stores the durable remaining delegation budget.
type AgentDepthStore interface {
	CreateTaskSessionWithAgentDepth(context.Context, string, string, string, int) (Session, error)
	ResolveAgentDepth(context.Context, string, int) (int, error)
	ResolveSavedAgentDepth(context.Context, string) (int, error)
	LowerAgentDepth(context.Context, string, int) error
}

func (s *service) CreateTaskSessionWithAgentDepth(ctx context.Context, callID, parentID, title string, remaining int) (Session, error) {
	if remaining < 0 || remaining > maxAgentDepth {
		return Session{}, fmt.Errorf("agent depth must be between 0 and %d", maxAgentDepth)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Session{}, fmt.Errorf("begin agent child creation: %w", err)
	}
	defer tx.Rollback()
	qtx := s.q.WithTx(tx)
	created, err := qtx.CreateSession(ctx, s.bindWorkspace(db.CreateSessionParams{
		ID: callID, ParentSessionID: sql.NullString{String: parentID, Valid: true},
		Title: title, CostParentID: parentID,
	}))
	if err != nil {
		if !isSessionsIDUniqueConstraintError(err) {
			return Session{}, err
		}
		created, err = qtx.GetSessionByID(ctx, callID)
		if err != nil {
			return Session{}, fmt.Errorf("re-read agent child session: %w", err)
		}
		if !created.ParentSessionID.Valid || created.ParentSessionID.String != parentID {
			return Session{}, errors.New("agent child session exists under another parent")
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO session_agent_depth(session_id, remaining_depth) VALUES (?, ?) ON CONFLICT(session_id) DO UPDATE SET remaining_depth = MIN(remaining_depth, excluded.remaining_depth)`, callID, remaining); err != nil {
		return Session{}, fmt.Errorf("persist agent child depth: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return Session{}, fmt.Errorf("commit agent child creation: %w", err)
	}
	result := s.fromDBItem(created)
	s.Publish(pubsub.CreatedEvent, result)
	return result, nil
}

// ResolveAgentDepth fails closed on missing or cyclic ancestry.
func (s *service) ResolveAgentDepth(ctx context.Context, sessionID string, rootCap int) (int, error) {
	if rootCap < 0 || rootCap > maxAgentDepth {
		return 0, fmt.Errorf("agent depth must be between 0 and %d", maxAgentDepth)
	}
	current := sessionID
	seen := make(map[string]struct{})
	remainingEdges := 0
	limit := rootCap
	hasSaved := false
	for current != "" {
		if _, exists := seen[current]; exists {
			return 0, errors.New("agent depth lineage cycle")
		}
		seen[current] = struct{}{}
		row, err := s.q.GetSessionByID(ctx, current)
		if err != nil {
			return 0, fmt.Errorf("agent depth lineage unavailable: %w", err)
		}
		var saved int
		err = s.db.QueryRowContext(ctx, `SELECT remaining_depth FROM session_agent_depth WHERE session_id = ?`, current).Scan(&saved)
		if err == nil {
			hasSaved = true
			if saved < 0 || saved > maxAgentDepth {
				return 0, errors.New("stored agent depth is invalid")
			}
			candidate := saved - remainingEdges
			if candidate < limit {
				limit = candidate
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("read stored agent depth: %w", err)
		}
		if !row.ParentSessionID.Valid || row.ParentSessionID.String == "" {
			if !hasSaved {
				candidate := defaultAgentDepth - remainingEdges
				if remainingEdges == 0 {
					candidate = rootCap
				}
				if candidate < limit {
					limit = candidate
				}
			}
			break
		}
		current = row.ParentSessionID.String
		remainingEdges++
	}
	if limit < 0 {
		return 0, nil
	}
	return limit, nil
}

// ResolveSavedAgentDepth returns the durable lineage cap without a new default.
func (s *service) ResolveSavedAgentDepth(ctx context.Context, sessionID string) (int, error) {
	current := sessionID
	seen := make(map[string]struct{})
	remainingEdges := 0
	limit := maxAgentDepth
	hasSaved := false
	for current != "" {
		if _, exists := seen[current]; exists {
			return 0, errors.New("agent depth lineage cycle")
		}
		seen[current] = struct{}{}
		row, err := s.q.GetSessionByID(ctx, current)
		if err != nil {
			return 0, fmt.Errorf("agent depth lineage unavailable: %w", err)
		}
		var saved int
		err = s.db.QueryRowContext(ctx, `SELECT remaining_depth FROM session_agent_depth WHERE session_id = ?`, current).Scan(&saved)
		if err == nil {
			hasSaved = true
			if saved < 0 || saved > maxAgentDepth {
				return 0, errors.New("stored agent depth is invalid")
			}
			limit = min(limit, saved-remainingEdges)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("read stored agent depth: %w", err)
		}
		if !row.ParentSessionID.Valid || row.ParentSessionID.String == "" {
			if !hasSaved {
				limit = defaultAgentDepth - remainingEdges
			}
			break
		}
		current = row.ParentSessionID.String
		remainingEdges++
	}
	return max(limit, 0), nil
}

// LowerAgentDepth only tightens the saved cap.
func (s *service) LowerAgentDepth(ctx context.Context, sessionID string, remaining int) error {
	if remaining < 0 || remaining > maxAgentDepth {
		return fmt.Errorf("agent depth must be between 0 and %d", maxAgentDepth)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO session_agent_depth(session_id, remaining_depth) VALUES (?, ?) ON CONFLICT(session_id) DO UPDATE SET remaining_depth = MIN(remaining_depth, excluded.remaining_depth)`, sessionID, remaining)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	return nil
}

var _ AgentDepthStore = (*service)(nil)
