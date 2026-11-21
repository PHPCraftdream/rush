package batchinst

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

type batchRoleAdmissionContextKey struct{}

// WithAdmittedBatchRoleContracts marks a batch tree after successful trusted
// admission so later internal translation preserves the pinned snapshots.
func WithAdmittedBatchRoleContracts(ctx context.Context) context.Context {
	return context.WithValue(ctx, batchRoleAdmissionContextKey{}, true)
}

func RoleContractsWereAdmitted(ctx context.Context) bool {
	admitted, _ := ctx.Value(batchRoleAdmissionContextKey{}).(bool)
	return admitted
}

func ConfigForBatchInvocation(cfg *config.Config) (*config.Config, error) {
	if cfg != nil {
		return cfg, nil
	}
	return nil, fmt.Errorf("%w: invocation config snapshot unavailable", agentcall.ErrAgentRoleUnconfigured)
}

func SpecHasCustomRole(spec jobbatch.Spec) bool {
	if spec.Task != nil {
		var leaf struct {
			Tool  string          `json:"tool"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(spec.Task.Payload, &leaf) != nil || leaf.Tool != agentToolName {
			return false
		}
		var fields struct {
			Role agentcall.RequestedModelRole `json:"role"`
		}
		if json.Unmarshal(leaf.Input, &fields) != nil {
			return false
		}
		return fields.Role != "" && !fields.Role.Builtin()
	}
	if spec.Batch != nil {
		for _, child := range spec.Batch.Items {
			if SpecHasCustomRole(child) {
				return true
			}
		}
	}
	return false
}

func ValidateSpecShapeForRoleAdmission(spec jobbatch.Spec) error {
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
		if task.Tool == agentToolName {
			if err := validateAgentRoleInput(task.Input); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range spec.Batch.Items {
		if err := ValidateSpecShapeForRoleAdmission(child); err != nil {
			return err
		}
	}
	return nil
}

func AdmitRoleContracts(cfg *config.Config, inherited *session.RoleContract, spec jobbatch.Spec, policy RoleAdmissionPolicy) (jobbatch.Spec, error) {
	if err := ValidateSpecShapeForRoleAdmission(spec); err != nil {
		return jobbatch.Spec{}, err
	}
	if inherited != nil {
		if err := inherited.Validate(); err != nil {
			return jobbatch.Spec{}, fmt.Errorf("%w: invalid inherited role contract: %v", agentcall.ErrAgentRoleUnconfigured, err)
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
		if leaf.Tool != agentToolName {
			if _, has := raw["selected_role_contract"]; has {
				return jobbatch.Spec{}, fmt.Errorf("%w: selected role snapshot on non-agent leaf", agentcall.ErrAgentRoleMismatch)
			}
			return spec, nil
		}
		var selected struct {
			Role agentcall.RequestedModelRole `json:"role"`
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
		if selected.Role == "" || selected.Role.Builtin() {
			if existing != nil {
				return jobbatch.Spec{}, fmt.Errorf("%w: custom snapshot supplied for a built-in or omitted role", agentcall.ErrAgentRoleMismatch)
			}
			if selected.Role != "" && cfg != nil && inherited == nil {
				if err := policy.ValidateBuiltin(cfg, selected.Role); err != nil {
					return jobbatch.Spec{}, err
				}
			}
			return spec, nil
		}
		if existing != nil {
			expected, err := policy.ResolveCustom(cfg, string(selected.Role), inherited)
			if err != nil {
				return jobbatch.Spec{}, err
			}
			if !existing.Equal(expected) {
				return jobbatch.Spec{}, fmt.Errorf("%w: selected role snapshot differs from configured admission", agentcall.ErrAgentRoleMismatch)
			}
			return spec, nil
		}
		contract, err := policy.ResolveCustom(cfg, string(selected.Role), inherited)
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
		child, err := AdmitRoleContracts(cfg, inherited, spec.Batch.Items[i], policy)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		batch.Items[i] = child
	}
	spec.Batch = &batch
	return spec, nil
}

func ValidateSelectedRole(contract *session.RoleContract, selected string, inherited *session.RoleContract) error {
	if contract == nil {
		return fmt.Errorf("%w: selected role snapshot is missing", agentcall.ErrAgentRoleUnconfigured)
	}
	if err := contract.Validate(); err != nil {
		return fmt.Errorf("%w: invalid selected role snapshot: %v", agentcall.ErrAgentRoleUnconfigured, err)
	}
	if contract.Name != selected || !ensureRoleContractGroupsPreserved(contract, inherited) {
		return fmt.Errorf("%w: selected role snapshot does not match role or inherited ceiling", agentcall.ErrAgentRoleMismatch)
	}
	return nil
}

func RoleContractInherits(child, parent *session.RoleContract) bool {
	if parent == nil {
		return true
	}
	if child == nil {
		return false
	}
	if err := parent.Validate(); err != nil {
		return false
	}
	want := parent.Inherited
	if parent.Tools != nil {
		want = append(slices.Clone(want), *parent.Tools)
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

func ValidateRolePayloads(spec jobbatch.Spec, decode func(json.RawMessage) (RolePayload, error)) error {
	if spec.Task != nil {
		p, err := decode(spec.Task.Payload)
		if err != nil {
			return err
		}
		if p.Parent != nil {
			if err := p.Parent.Validate(); err != nil {
				return fmt.Errorf("invalid batch parent role contract: %w", err)
			}
		}
		if p.Tool == agentToolName {
			if err := validateAgentRoleInput(p.Input); err != nil {
				return fmt.Errorf("batch role admission: %w", err)
			}
			var selected struct {
				Role agentcall.RequestedModelRole `json:"role"`
			}
			if err := json.Unmarshal(p.Input, &selected); err != nil {
				return err
			}
			if selected.Role != "" && !selected.Role.Builtin() && p.SelectedRoleContract == nil {
				return fmt.Errorf("%w: selected custom role snapshot is missing", agentcall.ErrAgentRoleUnconfigured)
			}
			if (selected.Role == "" || selected.Role.Builtin()) && p.SelectedRoleContract != nil {
				return fmt.Errorf("%w: custom snapshot supplied for built-in or omitted role", agentcall.ErrAgentRoleMismatch)
			}
		} else if p.SelectedRoleContract != nil {
			return fmt.Errorf("%w: selected role snapshot on non-agent leaf", agentcall.ErrAgentRoleMismatch)
		}
		if p.SelectedRoleContract != nil {
			if err := ValidateSelectedRole(p.SelectedRoleContract, p.SelectedRoleContract.Name, p.Parent); err != nil {
				return err
			}
			var input struct {
				Role agentcall.RequestedModelRole `json:"role"`
			}
			if json.Unmarshal(p.Input, &input) != nil || input.Role != agentcall.RequestedModelRole(p.SelectedRoleContract.Name) {
				return fmt.Errorf("%w: persisted selection/input mismatch", agentcall.ErrAgentRoleMismatch)
			}
		}
		return nil
	}
	if spec.Batch == nil {
		return errors.New("batch role payload has no node")
	}
	for _, child := range spec.Batch.Items {
		if err := ValidateRolePayloads(child, decode); err != nil {
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
	return RoleContractInherits(child, parent)
}

// RoleAdmissionPolicy resolves roles using the supplied invocation snapshot.
type RoleAdmissionPolicy struct {
	ValidateBuiltin func(*config.Config, agentcall.RequestedModelRole) error
	ResolveCustom   func(*config.Config, string, *session.RoleContract) (*session.RoleContract, error)
}

// RolePayload is the role-related projection of a decoded persisted leaf.
type RolePayload struct {
	Tool                 string
	Input                json.RawMessage
	Parent               *session.RoleContract
	SelectedRoleContract *session.RoleContract
}

func validateAgentRoleInput(input json.RawMessage) error {
	var fields struct {
		Role agentcall.RequestedModelRole `json:"role"`
	}
	if err := json.Unmarshal(input, &fields); err != nil {
		return fmt.Errorf("invalid agent role input: %w", err)
	}
	return nil
}

// AgentInjectMessage addresses an answer by depth and exact question sequence.
type AgentInjectMessage struct {
	Depth    int             `json:"depth"`
	Sequence uint64          `json:"sequence"`
	Text     json.RawMessage `json:"text,omitempty"`
}

var (
	// ErrBatchAgentNotHeld: no question is held, or the addressed depth or
	// published question sequence does not match the held one.
	ErrBatchAgentNotHeld = errors.New("batch agent: no held question at that depth")
	// ErrBatchAgentStopped: the incarnation is stopping or stopped; an
	// injected message would be lost, so it is rejected instead.
	ErrBatchAgentStopped = errors.New("batch agent: incarnation is stopped")
	// ErrBatchAgentStaleGeneration: the incarnation is terminal; a message
	// must never reach a new incarnation through the old handle.
	ErrBatchAgentStaleGeneration = errors.New("batch agent: stale incarnation")
	// ErrBatchAgentQuestionHeld: a second question cannot be held at the
	// same time.
	ErrBatchAgentQuestionHeld = errors.New("batch agent: question already held")
	// ErrBatchQuestionNoRoute rejects an ask without an established answer route.
	ErrBatchQuestionNoRoute = errors.New("batch agent: no answer route is bound for held questions (unattended batch)")
	// ErrBatchQuestionCapabilityNotDurable rejects queueing a live answer binding.
	ErrBatchQuestionCapabilityNotDurable = errors.New("batch agent: a call carrying batch question capability cannot be durably queued")
)
