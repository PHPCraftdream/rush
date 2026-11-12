package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrSessionRoleContractMismatch = errors.New("session_role_contract_mismatch")

// SessionRoleContractStore binds immutable role/tool/model snapshots to sessions.
type SessionRoleContractStore interface {
	GetSessionRoleContract(context.Context, string) (*RoleContract, bool, error)
	BindSessionRoleContract(context.Context, string, *RoleContract) error
}

func (s *service) GetSessionRoleContract(ctx context.Context, id string) (*RoleContract, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT contract_json FROM session_role_contracts WHERE session_id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read session role contract: %w", err)
	}
	contract, err := decodeRoleContract(raw)
	if err != nil {
		return nil, true, fmt.Errorf("decode session role contract: %w", err)
	}
	return contract, true, nil
}

func (s *service) BindSessionRoleContract(ctx context.Context, id string, contract *RoleContract) error {
	if err := contract.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(contract.Clone())
	if err != nil {
		return fmt.Errorf("encode session role contract: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin session role contract binding: %w", err)
	}
	defer tx.Rollback()
	var found bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)`, id).Scan(&found); err != nil {
		return fmt.Errorf("check session for role contract binding: %w", err)
	}
	if !found {
		return sql.ErrNoRows
	}
	var legacy string
	err = tx.QueryRowContext(ctx, `SELECT requested_role FROM session_agent_model_role WHERE session_id = ?`, id).Scan(&legacy)
	legacyExists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read legacy role before contract binding: %w", err)
	}
	if legacyExists && legacy != contract.Name {
		return ErrSessionRoleContractMismatch
	}
	if legacyExists {
		var existingContract string
		existingErr := tx.QueryRowContext(ctx, `SELECT contract_json FROM session_role_contracts WHERE session_id = ?`, id).Scan(&existingContract)
		if existingErr == nil {
			existing, decodeErr := decodeRoleContract(existingContract)
			if decodeErr != nil {
				return fmt.Errorf("decode bound session role contract: %w", decodeErr)
			}
			if !existing.Equal(contract) {
				return ErrSessionRoleContractMismatch
			}
			return tx.Commit()
		}
		if !errors.Is(existingErr, sql.ErrNoRows) {
			return fmt.Errorf("read bound session role contract: %w", existingErr)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_role_contracts(session_id, contract_json) VALUES (?, ?) ON CONFLICT(session_id) DO NOTHING`, id, string(encoded)); err != nil {
		return fmt.Errorf("bind session role contract: %w", err)
	}
	var stored string
	if err := tx.QueryRowContext(ctx, `SELECT contract_json FROM session_role_contracts WHERE session_id = ?`, id).Scan(&stored); err != nil {
		return fmt.Errorf("read bound session role contract: %w", err)
	}
	existing, err := decodeRoleContract(stored)
	if err != nil {
		return fmt.Errorf("decode bound session role contract: %w", err)
	}
	if !existing.Equal(contract) {
		return ErrSessionRoleContractMismatch
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session role contract binding: %w", err)
	}
	return nil
}

var _ SessionRoleContractStore = (*service)(nil)
