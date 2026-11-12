package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

type batchRoleAdmissionContextKey struct{}

// WithAdmittedBatchRoleContracts marks a batch tree after successful trusted
// admission so later internal translation preserves the pinned snapshots.
func WithAdmittedBatchRoleContracts(ctx context.Context) context.Context {
	return context.WithValue(ctx, batchRoleAdmissionContextKey{}, true)
}

func batchRoleContractsWereAdmitted(ctx context.Context) bool {
	admitted, _ := ctx.Value(batchRoleAdmissionContextKey{}).(bool)
	return admitted
}

// AdmitBatchRoleContracts resolves every explicitly selected custom role in a
// structural batch before the caller can reserve capacity or construct tools.
// The returned payloads contain immutable role snapshots used by later queue
// execution; config is read only from the coordinator's pinned snapshot.
func AdmitBatchRoleContracts(ctx context.Context, c Coordinator, spec jobbatch.Spec) (jobbatch.Spec, error) {
	if err := validateBatchSpecShapeForRoleAdmission(spec); err != nil {
		return jobbatch.Spec{}, err
	}
	if batchRoleContractsWereAdmitted(ctx) {
		if err := validateBatchRolePayloads(spec); err != nil {
			return jobbatch.Spec{}, err
		}
		return spec, nil
	}
	if cfg, ok := parentRoleConfigFrom(ctx); ok {
		admitted, err := admitBatchRoleContracts(cfg, parentRoleContractFrom(ctx), spec)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		return admitted, nil
	}
	if cc, ok := c.(*coordinator); ok && cc.cfg != nil {
		cfg, _ := cc.cfg.Snapshot()
		return admitBatchRoleContracts(cfg, parentRoleContractFrom(ctx), spec)
	}
	if batchSpecHasCustomRole(spec) {
		return jobbatch.Spec{}, fmt.Errorf("%w: custom role configuration unavailable", ErrAgentRoleUnconfigured)
	}
	return spec, nil
}

func configForBatchInvocation(inv BatchInvocation) (*config.Config, error) {
	if inv.roleConfig != nil {
		return inv.roleConfig, nil
	}
	return nil, fmt.Errorf("%w: invocation config snapshot unavailable", ErrAgentRoleUnconfigured)
}

func batchSpecHasCustomRole(spec jobbatch.Spec) bool {
	if spec.Task != nil {
		var leaf struct {
			Tool  string          `json:"tool"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(spec.Task.Payload, &leaf) != nil || leaf.Tool != AgentToolName {
			return false
		}
		var fields struct {
			Role RequestedModelRole `json:"role"`
		}
		if json.Unmarshal(leaf.Input, &fields) != nil {
			return false
		}
		return fields.Role != "" && !fields.Role.builtin()
	}
	if spec.Batch != nil {
		for _, child := range spec.Batch.Items {
			if batchSpecHasCustomRole(child) {
				return true
			}
		}
	}
	return false
}

func validateBatchSpecShapeForRoleAdmission(spec jobbatch.Spec) error {
	if (spec.Task == nil) == (spec.Batch == nil) {
		return errors.New("batch role admission: every item must be exactly a task or batch")
	}
	if spec.Task != nil {
		var task struct {
			Tool  string          `json:"tool"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(spec.Task.Payload, &task); err != nil {
			return fmt.Errorf("batch role admission: invalid task payload: %w", err)
		}
		if task.Tool == AgentToolName {
			if err := validateAgentRoleInput(task.Input); err != nil {
				return err
			}
			var selected struct {
				Role RequestedModelRole `json:"role"`
			}
			if err := json.Unmarshal(task.Input, &selected); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range spec.Batch.Items {
		if err := validateBatchSpecShapeForRoleAdmission(child); err != nil {
			return err
		}
	}
	return nil
}

func admitBatchRoleContracts(cfg *config.Config, inherited *session.RoleContract, spec jobbatch.Spec) (jobbatch.Spec, error) {
	if err := validateBatchSpecShapeForRoleAdmission(spec); err != nil {
		return jobbatch.Spec{}, err
	}
	if inherited != nil {
		if err := inherited.Validate(); err != nil {
			return jobbatch.Spec{}, fmt.Errorf("%w: invalid inherited role contract: %v", ErrAgentRoleUnconfigured, err)
		}
	}
	if spec.Task != nil {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(spec.Task.Payload, &raw); err != nil {
			return jobbatch.Spec{}, fmt.Errorf("batch role admission: invalid task payload: %w", err)
		}
		var leaf struct {
			Tool  string          `json:"tool"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(spec.Task.Payload, &leaf); err != nil {
			return jobbatch.Spec{}, err
		}
		if leaf.Tool != AgentToolName {
			if _, has := raw["selected_role_contract"]; has {
				return jobbatch.Spec{}, fmt.Errorf("%w: selected role snapshot on non-agent leaf", ErrAgentRoleMismatch)
			}
			return spec, nil
		}
		var selected struct {
			Role RequestedModelRole `json:"role"`
		}
		if err := json.Unmarshal(leaf.Input, &selected); err != nil {
			return jobbatch.Spec{}, err
		}
		var existing *session.RoleContract
		if encoded := raw["selected_role_contract"]; len(encoded) > 0 {
			if err := json.Unmarshal(encoded, &existing); err != nil {
				return jobbatch.Spec{}, fmt.Errorf("batch role admission: invalid selected role snapshot: %w", err)
			}
		}
		if selected.Role == "" || selected.Role.builtin() {
			if existing != nil {
				return jobbatch.Spec{}, fmt.Errorf("%w: custom snapshot supplied for a built-in or omitted role", ErrAgentRoleMismatch)
			}
			if selected.Role != "" && cfg != nil && inherited == nil {
				if _, _, err := requestedRoleSlot(cfg, session.Session{}, selected.Role); err != nil {
					return jobbatch.Spec{}, err
				}
			}
			return spec, nil
		}
		if existing != nil {
			expected, err := ResolveRoleContract(cfg, string(selected.Role), inherited)
			if err != nil {
				return jobbatch.Spec{}, err
			}
			if !existing.Equal(expected) {
				return jobbatch.Spec{}, fmt.Errorf("%w: selected role snapshot differs from configured admission", ErrAgentRoleMismatch)
			}
			return spec, nil
		}
		contract, err := ResolveRoleContract(cfg, string(selected.Role), inherited)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		encoded, err := json.Marshal(contract)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		raw["selected_role_contract"] = encoded
		payload, err := json.Marshal(raw)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		task := *spec.Task
		task.Payload = payload
		spec.Task = &task
		return spec, nil
	}
	batch := *spec.Batch
	batch.Items = make([]jobbatch.Spec, len(spec.Batch.Items))
	for i := range spec.Batch.Items {
		child, err := admitBatchRoleContracts(cfg, inherited, spec.Batch.Items[i])
		if err != nil {
			return jobbatch.Spec{}, err
		}
		batch.Items[i] = child
	}
	spec.Batch = &batch
	return spec, nil
}

func validateSelectedBatchRole(contract *session.RoleContract, selected string, inherited *session.RoleContract) error {
	if contract == nil {
		return fmt.Errorf("%w: selected role snapshot is missing", ErrAgentRoleUnconfigured)
	}
	if err := contract.Validate(); err != nil {
		return fmt.Errorf("%w: invalid selected role snapshot: %v", ErrAgentRoleUnconfigured, err)
	}
	if contract.Name != selected || !roleContractInherits(contract, inherited) {
		return fmt.Errorf("%w: selected role snapshot does not match role or inherited ceiling", ErrAgentRoleMismatch)
	}
	return nil
}

func roleContractInherits(child, parent *session.RoleContract) bool {
	if parent == nil {
		return true
	}
	if child == nil {
		return false
	}
	want, err := inheritedRoleCeilings(parent)
	if err != nil {
		return false
	}
	if len(child.Inherited) < len(want) {
		return false
	}
	for i := range want {
		if !slices.Equal(child.Inherited[i], want[i]) {
			return false
		}
	}
	return true
}

func validateBatchRolePayloads(spec jobbatch.Spec) error {
	if spec.Task != nil {
		var p batchLeafPayload
		if err := json.Unmarshal(spec.Task.Payload, &p); err != nil {
			return err
		}
		if p.Policy != nil && p.Policy.RoleContract != nil {
			if err := p.Policy.RoleContract.Validate(); err != nil {
				return fmt.Errorf("invalid batch parent role contract: %w", err)
			}
		}
		if p.Tool == AgentToolName {
			if err := validateAgentRoleInput(p.Input); err != nil {
				return fmt.Errorf("batch role admission: %w", err)
			}
			var selected struct {
				Role RequestedModelRole `json:"role"`
			}
			if err := json.Unmarshal(p.Input, &selected); err != nil {
				return err
			}
			if selected.Role != "" && !selected.Role.builtin() && p.SelectedRoleContract == nil {
				return fmt.Errorf("%w: selected custom role snapshot is missing", ErrAgentRoleUnconfigured)
			}
			if (selected.Role == "" || selected.Role.builtin()) && p.SelectedRoleContract != nil {
				return fmt.Errorf("%w: custom snapshot supplied for built-in or omitted role", ErrAgentRoleMismatch)
			}
		} else if p.SelectedRoleContract != nil {
			return fmt.Errorf("%w: selected role snapshot on non-agent leaf", ErrAgentRoleMismatch)
		}
		if p.SelectedRoleContract != nil {
			if err := validateSelectedBatchRole(p.SelectedRoleContract, p.SelectedRoleContract.Name, func() *session.RoleContract {
				if p.Policy == nil {
					return nil
				}
				return p.Policy.RoleContract
			}()); err != nil {
				return err
			}
			var input struct {
				Role RequestedModelRole `json:"role"`
			}
			if json.Unmarshal(p.Input, &input) != nil || input.Role != RequestedModelRole(p.SelectedRoleContract.Name) {
				return fmt.Errorf("%w: persisted selection/input mismatch", ErrAgentRoleMismatch)
			}
		}
		return nil
	}
	if spec.Batch == nil {
		return errors.New("batch role payload has no node")
	}
	for _, child := range spec.Batch.Items {
		if err := validateBatchRolePayloads(child); err != nil {
			return err
		}
	}
	return nil
}

// BatchRoleSnapshotHash returns a stable hash of selected role contracts in one tree.
func BatchRoleSnapshotHash(spec jobbatch.Spec) string {
	var snapshots []any
	var walk func(jobbatch.Spec)
	walk = func(node jobbatch.Spec) {
		if node.Task != nil {
			var leaf struct {
				Tool     string                `json:"tool"`
				Contract *session.RoleContract `json:"selected_role_contract"`
			}
			if json.Unmarshal(node.Task.Payload, &leaf) == nil && leaf.Contract != nil {
				snapshots = append(snapshots, struct {
					Tool     string                `json:"tool"`
					Contract *session.RoleContract `json:"contract"`
				}{leaf.Tool, leaf.Contract})
			}
			return
		}
		if node.Batch != nil {
			for _, child := range node.Batch.Items {
				walk(child)
			}
		}
	}
	walk(spec)
	raw, _ := json.Marshal(snapshots)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ensureRoleContractGroupsPreserved(child, parent *session.RoleContract) bool {
	return roleContractInherits(child, parent)
}
