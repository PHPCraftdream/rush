package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrAgentModelRoleMismatch = errors.New("agent_role_mismatch")

// AgentModelRoleStore preserves the immutable model-only delegation contract.
type AgentModelRoleStore interface {
	GetAgentModelRole(context.Context, string) (string, bool, error)
	BindAgentModelRole(context.Context, string, string) error
}

func (s *service) GetAgentModelRole(ctx context.Context, id string) (string, bool, error) {
	var role string
	err := s.db.QueryRowContext(ctx, `SELECT requested_role FROM session_agent_model_role WHERE session_id = ?`, id).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read delegated model role: %w", err)
	}
	return role, true, nil
}

func (s *service) BindAgentModelRole(ctx context.Context, id, role string) error {
	switch role {
	case "", "smart", "fast", "worker", "reviewer":
	default:
		return errors.New("agent_role_invalid")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO session_agent_model_role(session_id, requested_role) VALUES (?, ?) ON CONFLICT(session_id) DO NOTHING`, id, role); err != nil {
		return fmt.Errorf("bind delegated model role: %w", err)
	}
	stored, found, err := s.GetAgentModelRole(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return sql.ErrNoRows
	}
	if stored != role {
		return ErrAgentModelRoleMismatch
	}
	return nil
}

var _ AgentModelRoleStore = (*service)(nil)
