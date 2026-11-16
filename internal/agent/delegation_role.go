package agent

import (
	"encoding/json"
	"fmt"
)

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
