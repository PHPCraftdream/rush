package server

import (
	"github.com/PHPCraftdream/rush/internal/config"
)

// validateAPIRunSelection resolves one immutable config snapshot before any
// run registry, policy gate, or coordinator admission is changed.
func validateAPIRunSelection(cfg *config.Config, role, model string) (config.SelectedModelType, error) {
	roleType, _, _, err := apiRunBackendSelection(cfg, role, model, nil)
	return roleType, err
}
