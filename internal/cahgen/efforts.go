package cahgen

import (
	"fmt"
	"slices"
)

// validateAcceptedEfforts checks raw aliases before collision/cap filtering.
func validateAcceptedEfforts(m Manifest, overrides map[string]Override) error {
	if m.AcceptedEfforts == nil {
		return nil
	}
	models := map[string]bool{}
	for _, e := range m.Codex {
		models[e.Model] = true
	}
	if len(models) != len(m.AcceptedEfforts) {
		return fmt.Errorf("acceptedEfforts model set mismatch")
	}
	for model, efforts := range m.AcceptedEfforts {
		if !models[model] || !codexRE.MatchString(model) || len(efforts) == 0 {
			return fmt.Errorf("invalid acceptedEfforts model %s", model)
		}
		for i, effort := range efforts {
			if rank(effort) < 0 || i > 0 && rank(efforts[i-1]) >= rank(effort) {
				return fmt.Errorf("invalid acceptedEfforts set %s", model)
			}
		}
		if o, ok := overrides[model]; ok && len(o.EffortCaps) > 0 && !slices.Equal(efforts, o.EffortCaps) {
			return fmt.Errorf("acceptedEfforts measured cap mismatch %s", model)
		}
	}
	for _, e := range m.Codex {
		if !slices.Contains(m.AcceptedEfforts[e.Model], e.Effort) {
			return fmt.Errorf("acceptedEfforts alias mismatch %s", e.Name)
		}
	}
	return nil
}
