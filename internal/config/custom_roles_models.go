package config

import (
	"fmt"
	"slices"
)

// ValidateCustomRoleSelection checks a role against one pinned provider snapshot.
// Unknown raw model IDs retain the existing unverified passthrough behavior.
func ValidateCustomRoleSelection(cfg *Config, role CustomRole) error {
	if err := role.Validate(); err != nil {
		return err
	}
	if cfg == nil || cfg.Providers == nil {
		return fmt.Errorf("model providers are not configured")
	}
	provider, exists := cfg.Providers.Get(role.Provider)
	if !exists {
		return fmt.Errorf("provider %q is not configured", role.Provider)
	}
	if role.ReasoningEffort == "" {
		return nil
	}
	var levels []string
	if live, reported := provider.LiveEfforts[role.Model]; reported {
		levels = live.Levels
	} else {
		for _, model := range provider.Models {
			if model.ID == role.Model {
				levels = model.ReasoningLevels
				break
			}
		}
	}
	if len(levels) != 0 && !slices.Contains(levels, role.ReasoningEffort) {
		return fmt.Errorf("effort %q is not supported by %s/%s (supported: %v)", role.ReasoningEffort, role.Provider, role.Model, levels)
	}
	return nil
}
