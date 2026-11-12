package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// SessionRootRoleContractStore tracks the current logical role execution for a
// root session; unlike child bindings, a new logical call replaces the prior one.
type SessionRootRoleContractStore interface {
	GetRootRoleContract(context.Context, string) (logicalID string, contract *RoleContract, found bool, err error)
	BindRootRoleContract(context.Context, string, string, *RoleContract) error
}

func (s *service) GetRootRoleContract(ctx context.Context, sessionID string) (string, *RoleContract, bool, error) {
	var logicalID string
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT logical_call_id, contract_json FROM session_root_role_contracts WHERE session_id = ?`, sessionID).Scan(&logicalID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("read root role contract: %w", err)
	}
	if logicalID == "" {
		return "", nil, true, errors.New("root role contract logical call id is empty")
	}
	if !raw.Valid {
		return logicalID, nil, true, nil
	}
	contract, err := decodeRoleContract(raw.String)
	if err != nil {
		return logicalID, nil, true, fmt.Errorf("decode root role contract: %w", err)
	}
	return logicalID, contract, true, nil
}

func (s *service) BindRootRoleContract(ctx context.Context, sessionID, logicalID string, contract *RoleContract) error {
	if logicalID == "" {
		return errors.New("root role contract logical call id is required")
	}
	var foundSession bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)`, sessionID).Scan(&foundSession); err != nil {
		return fmt.Errorf("check root session for role contract: %w", err)
	}
	if !foundSession {
		return sql.ErrNoRows
	}
	var encoded any
	if contract != nil {
		if err := contract.Validate(); err != nil {
			return err
		}
		data, err := json.Marshal(contract.Clone())
		if err != nil {
			return fmt.Errorf("encode root role contract: %w", err)
		}
		encoded = string(data)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin root role contract binding: %w", err)
	}
	defer tx.Rollback()
	var previousID string
	var previousRaw sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT logical_call_id, contract_json FROM session_root_role_contracts WHERE session_id = ?`, sessionID).Scan(&previousID, &previousRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read existing root role contract: %w", err)
	}
	if err == nil && previousID == logicalID && previousRaw.Valid {
		if _, decodeErr := decodeRoleContract(previousRaw.String); decodeErr != nil {
			return fmt.Errorf("decode existing root role contract: %w", decodeErr)
		}
	}
	if err == nil && previousID == logicalID {
		var previous *RoleContract
		if previousRaw.Valid {
			previous, err = decodeRoleContract(previousRaw.String)
			if err != nil {
				return fmt.Errorf("decode existing root role contract: %w", err)
			}
		}
		if !previous.Equal(contract) {
			return ErrSessionRoleContractMismatch
		}
		return tx.Commit()
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO session_root_role_contracts(session_id, logical_call_id, contract_json)
		VALUES (?, ?, ?) ON CONFLICT(session_id) DO UPDATE SET logical_call_id=excluded.logical_call_id, contract_json=excluded.contract_json
		WHERE session_root_role_contracts.logical_call_id <> excluded.logical_call_id`, sessionID, logicalID, encoded); err != nil {
		return fmt.Errorf("bind root role contract: %w", err)
	}
	var currentID string
	var currentRaw sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT logical_call_id, contract_json FROM session_root_role_contracts WHERE session_id = ?`, sessionID).Scan(&currentID, &currentRaw); err != nil {
		return fmt.Errorf("read bound root role contract: %w", err)
	}
	if currentID != logicalID {
		return ErrSessionRoleContractMismatch
	}
	var current *RoleContract
	if currentRaw.Valid {
		current, err = decodeRoleContract(currentRaw.String)
		if err != nil {
			return fmt.Errorf("decode current root role contract: %w", err)
		}
	}
	if !current.Equal(contract) {
		return ErrSessionRoleContractMismatch
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit root role contract binding: %w", err)
	}
	return nil
}

var _ SessionRootRoleContractStore = (*service)(nil)
