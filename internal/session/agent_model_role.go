package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrAgentModelRoleMismatch = errors.New("agent_role_mismatch")

// AgentModelRoleStore preserves immutable delegated role identities. Built-in and
// empty roles use the legacy model-only table; custom names require the full
// immutable RoleContract snapshot in session_role_contracts.
type AgentModelRoleStore interface {
	GetAgentModelRole(context.Context, string) (string, bool, error)
	BindAgentModelRole(context.Context, string, string) error
}

func (s *service) GetAgentModelRole(ctx context.Context, id string) (string, bool, error) {
	contract, contractFound, contractErr := s.GetSessionRoleContract(ctx, id)
	if contractErr != nil {
		return "", false, contractErr
	}
	if contractFound {
		return contract.Name, true, nil
	}
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
	builtin := role == "" || role == "smart" || role == "fast" || role == "worker" || role == "reviewer"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delegated model role binding: %w", err)
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT contract_json FROM session_role_contracts WHERE session_id = ?`, id).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read delegated role contract: %w", err)
	}
	if err == nil {
		contract, decodeErr := decodeRoleContract(raw)
		if decodeErr != nil {
			return fmt.Errorf("decode delegated role contract: %w", decodeErr)
		}
		if contract.Name != role {
			return ErrAgentModelRoleMismatch
		}
		return tx.Commit()
	}
	if !builtin {
		return errors.New("agent_role_invalid")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_agent_model_role(session_id, requested_role) VALUES (?, ?) ON CONFLICT(session_id) DO NOTHING`, id, role); err != nil {
		return fmt.Errorf("bind delegated model role: %w", err)
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT requested_role FROM session_agent_model_role WHERE session_id = ?`, id).Scan(&stored); err != nil {
		return fmt.Errorf("read delegated model role: %w", err)
	}
	if stored != role {
		return ErrAgentModelRoleMismatch
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delegated model role binding: %w", err)
	}
	return nil
}

var _ AgentModelRoleStore = (*service)(nil)
