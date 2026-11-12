package agent

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
)

// RequestedModelRole selects only a delegation's model, never its tool policy.
type RequestedModelRole string

var (
	ErrAgentRoleInvalid      = errors.New("agent_role_invalid")
	ErrAgentRoleUnconfigured = errors.New("agent_role_unconfigured")
	ErrAgentRoleMismatch     = errors.New("agent_role_mismatch")
)

func (r RequestedModelRole) valid() bool {
	switch config.SelectedModelType(r) {
	case config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
		return true
	default:
		return config.ValidateCustomRoleName(string(r)) == nil
	}
}

func (r RequestedModelRole) builtin() bool {
	switch config.SelectedModelType(r) {
	case config.SelectedModelTypeSmart, config.SelectedModelTypeFast, config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
		return true
	default:
		return false
	}
}

// UnmarshalJSON distinguishes an omitted legacy role from explicit empty/null.
func (r *RequestedModelRole) UnmarshalJSON(raw []byte) error {
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		return fmt.Errorf("%w: expected a built-in or valid custom role name", ErrAgentRoleInvalid)
	}
	role := RequestedModelRole(name)
	if !role.valid() {
		return fmt.Errorf("%w: expected a built-in or valid custom role name", ErrAgentRoleInvalid)
	}
	*r = role
	return nil
}

// validateAgentRoleInput checks model-only control fields before batch admission.
func validateAgentRoleInput(input json.RawMessage) error {
	var fields struct {
		Role RequestedModelRole `json:"role"`
	}
	if err := json.Unmarshal(input, &fields); err != nil {
		return fmt.Errorf("invalid agent role input: %w", err)
	}
	return nil
}
