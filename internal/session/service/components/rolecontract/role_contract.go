package rolecontract

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/session/service/components/sesstypes"
)

func GetSessionRoleContract(ctx context.Context, db *sql.DB, id string) (*sesstypes.RoleContract, bool, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT contract_json FROM session_role_contracts WHERE session_id = ?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read session role contract: %w", err)
	}
	contract, err := sesstypes.DecodeRoleContract(raw)
	if err != nil {
		return nil, true, fmt.Errorf("decode session role contract: %w", err)
	}
	return contract, true, nil
}

func BindSessionRoleContract(ctx context.Context, db *sql.DB, id string, contract *sesstypes.RoleContract, mismatch error) error {
	if err := contract.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(contract.Clone())
	if err != nil {
		return fmt.Errorf("encode session role contract: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
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
		return mismatch
	}
	if legacyExists {
		var existingContract string
		existingErr := tx.QueryRowContext(ctx, `SELECT contract_json FROM session_role_contracts WHERE session_id = ?`, id).Scan(&existingContract)
		if existingErr == nil {
			existing, decodeErr := sesstypes.DecodeRoleContract(existingContract)
			if decodeErr != nil {
				return fmt.Errorf("decode bound session role contract: %w", decodeErr)
			}
			if !existing.Equal(contract) {
				return mismatch
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
	existing, err := sesstypes.DecodeRoleContract(stored)
	if err != nil {
		return fmt.Errorf("decode bound session role contract: %w", err)
	}
	if !existing.Equal(contract) {
		return mismatch
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session role contract binding: %w", err)
	}
	return nil
}

func GetRootRoleContract(ctx context.Context, db *sql.DB, sessionID string) (string, *sesstypes.RoleContract, bool, error) {
	var logicalID string
	var raw sql.NullString
	err := db.QueryRowContext(ctx, `SELECT logical_call_id, contract_json FROM session_root_role_contracts WHERE session_id = ?`, sessionID).Scan(&logicalID, &raw)
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
	contract, err := sesstypes.DecodeRoleContract(raw.String)
	if err != nil {
		return logicalID, nil, true, fmt.Errorf("decode root role contract: %w", err)
	}
	return logicalID, contract, true, nil
}

func BindRootRoleContract(ctx context.Context, db *sql.DB, sessionID, logicalID string, contract *sesstypes.RoleContract, mismatch error) error {
	if logicalID == "" {
		return errors.New("root role contract logical call id is required")
	}
	var foundSession bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE id = ?)`, sessionID).Scan(&foundSession); err != nil {
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
	tx, err := db.BeginTx(ctx, nil)
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
		if _, decodeErr := sesstypes.DecodeRoleContract(previousRaw.String); decodeErr != nil {
			return fmt.Errorf("decode existing root role contract: %w", decodeErr)
		}
	}
	if err == nil && previousID == logicalID {
		var previous *sesstypes.RoleContract
		if previousRaw.Valid {
			previous, err = sesstypes.DecodeRoleContract(previousRaw.String)
			if err != nil {
				return fmt.Errorf("decode existing root role contract: %w", err)
			}
		}
		if !previous.Equal(contract) {
			return mismatch
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
		return mismatch
	}
	var current *sesstypes.RoleContract
	if currentRaw.Valid {
		current, err = sesstypes.DecodeRoleContract(currentRaw.String)
		if err != nil {
			return fmt.Errorf("decode current root role contract: %w", err)
		}
	}
	if !current.Equal(contract) {
		return mismatch
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit root role contract binding: %w", err)
	}
	return nil
}

func GetAgentModelRole(ctx context.Context, db *sql.DB, id string) (string, bool, error) {
	contract, contractFound, contractErr := GetSessionRoleContract(ctx, db, id)
	if contractErr != nil {
		return "", false, contractErr
	}
	if contractFound {
		return contract.Name, true, nil
	}
	var role string
	err := db.QueryRowContext(ctx, `SELECT requested_role FROM session_agent_model_role WHERE session_id = ?`, id).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read delegated model role: %w", err)
	}
	return role, true, nil
}

func BindAgentModelRole(ctx context.Context, db *sql.DB, id, role string, mismatch error) error {
	builtin := role == "" || role == "smart" || role == "fast" || role == "worker" || role == "reviewer"
	tx, err := db.BeginTx(ctx, nil)
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
		contract, decodeErr := sesstypes.DecodeRoleContract(raw)
		if decodeErr != nil {
			return fmt.Errorf("decode delegated role contract: %w", decodeErr)
		}
		if contract.Name != role {
			return mismatch
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
		return mismatch
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delegated model role binding: %w", err)
	}
	return nil
}
