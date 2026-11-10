package server

import (
	"fmt"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
)

// validateAPIRunSelection resolves one immutable config snapshot before any
// run registry, policy gate, or coordinator admission is changed.
func validateAPIRunSelection(cfg *config.Config, role, model string) (config.SelectedModelType, error) {
	roleType, err := apiRunRole(role)
	if err != nil {
		return "", err
	}
	if role != "" && roleType == "" {
		if model != "" {
			return "", ErrAPIRunBackendInvalidModel
		}
		if _, err := agent.ResolveRoleContract(cfg, role, nil); err != nil {
			return "", fmt.Errorf("%w: %v", ErrAPIRunBackendInvalidRole, err)
		}
		return "", nil
	}
	if model != "" {
		if _, err := apiRunModelOverride(cfg, model); err != nil {
			return "", err
		}
	}
	return roleType, nil
}
