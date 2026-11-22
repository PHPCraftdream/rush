package batchflow

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	sessionservice "github.com/PHPCraftdream/rush/internal/session/service"
)

// ErrBatchCLIOutputExpired reports that a released leaf's output is no
// longer cached; its persisted outcome is still valid.
var ErrBatchCLIOutputExpired = errors.New("batch CLI: output expired or evicted")

// OwnerLeafFacts is the decoded host-owned leaf payload slice the CLI
// admission checks need. The caller owns the wire struct and decodes it,
// keeping batchflow free of agent-internal payload types and of the tool-name table (AgentTool is whether the leaf is the sub-agent tool).
type OwnerLeafFacts struct {
	SessionID            string
	AgentTool            bool
	Input                json.RawMessage
	SelectedRoleContract *sessionservice.RoleContract
}

// ValidateBatchOwnerPayloads walks the spec tree and checks every task leaf:
// the decoded payload's session identity must match the submit owner, and an
// agent leaf's role selection must be internally consistent. decode converts
// one raw leaf payload into OwnerLeafFacts.
func ValidateBatchOwnerPayloads(owner string, spec jobbatch.Spec, decode func(json.RawMessage) (OwnerLeafFacts, error)) error {
	if (spec.Task == nil) == (spec.Batch == nil) {
		return errors.New("batch CLI: every item must be exactly a task or batch")
	}
	if spec.Task != nil {
		payload, err := decode(spec.Task.Payload)
		if err != nil || payload.SessionID != owner {
			return errors.New("batch CLI: task owner identity does not match submit owner")
		}
		if payload.AgentTool {
			var selected struct {
				Role agentcall.RequestedModelRole `json:"role"`
			}
			if err := json.Unmarshal(payload.Input, &selected); err != nil {
				return err
			}
			if selected.Role != "" && !selected.Role.Builtin() && payload.SelectedRoleContract == nil {
				return fmt.Errorf("%w: role snapshot was not admitted", agentcall.ErrAgentRoleUnconfigured)
			}
			if selected.Role.Builtin() && payload.SelectedRoleContract != nil {
				return fmt.Errorf("%w: custom snapshot does not match builtin role", agentcall.ErrAgentRoleMismatch)
			}
		}
		return nil
	}
	for _, child := range spec.Batch.Items {
		if err := ValidateBatchOwnerPayloads(owner, child, decode); err != nil {
			return err
		}
	}
	return nil
}

// IsTerminalBatchState reports whether a batch node state is settled.
func IsTerminalBatchState(state jobbatch.State) bool {
	switch state {
	case jobbatch.StateCompleted, jobbatch.StateFailed, jobbatch.StateCancelled, jobbatch.StateTimedOut, jobbatch.StateInterrupted, jobbatch.StateSkipped:
		return true
	}
	return false
}
