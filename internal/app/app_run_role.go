package app

import (
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
)

// resolveRunRole freezes a custom role for one logical execution. A caller
// without an existing snapshot (every new SDK/CLI/API execution) resolves from
// the current pinned config; subsequent turns reuse the immutable snapshot.
func (app *App) resolveRunRole(overrides *RunOverrides) error {
	if overrides == nil {
		return nil
	}
	if overrides.RequestedRole == "" {
		switch overrides.ModelRole {
		case "", config.SelectedModelTypeSmart, config.SelectedModelTypeFast,
			config.SelectedModelTypeWorker, config.SelectedModelTypeReviewer:
			return nil
		default:
			return fmt.Errorf("ModelRole %q is not a built-in role; use RequestedRole", overrides.ModelRole)
		}
	}
	if overrides.ModelRole != "" {
		return fmt.Errorf("custom RequestedRole cannot be combined with built-in ModelRole")
	}
	if overrides.SmartModel != "" {
		return fmt.Errorf("custom RequestedRole cannot be combined with SmartModel")
	}
	if overrides.roleContract != nil {
		if err := overrides.roleContract.Validate(); err != nil || overrides.roleContract.Name != overrides.RequestedRole {
			return fmt.Errorf("invalid snapshot for requested role %q", overrides.RequestedRole)
		}
		return nil
	}
	cfg, _ := app.config.Snapshot()
	contract, err := agent.ResolveRoleContract(cfg, overrides.RequestedRole, nil)
	if err != nil {
		return fmt.Errorf("resolve custom role %q: %w", overrides.RequestedRole, err)
	}
	if overrides.ReasoningEffort != "" {
		contract.Model.ReasoningEffort = overrides.ReasoningEffort
	}
	overrides.roleContract = contract.Clone()
	return nil
}

// runRoleName is the requested role label used by resume/error diagnostics.
func runRoleName(overrides RunOverrides) string {
	if overrides.RequestedRole != "" {
		return overrides.RequestedRole
	}
	return string(overrides.ModelRole)
}

// shellQuoteRunRole quotes an exact role name for the POSIX resume command.
func shellQuoteRunRole(role string) string {
	if role == "" {
		return "smart"
	}
	for _, r := range role {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == '@' || r == '/' || r == ':' || r == '+' {
			continue
		}
		return "'" + strings.ReplaceAll(role, "'", `'\''`) + "'"
	}
	return role
}
