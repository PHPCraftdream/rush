package cmd

import (
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

// writePeakEntry keeps workspace off explicit instead of restoring inheritance.
func writePeakEntry(a *app.App, scope config.Scope, id string, entry *config.ProviderPeakHours) error {
	if err := entry.Validate(); err != nil {
		return err
	}
	normalized := entry.NormalizeForWrite()
	if normalized == nil && scope == config.ScopeGlobal {
		return a.Store().RemoveConfigField(scope, peakFieldPath(id))
	}
	return a.Store().SetConfigField(scope, peakFieldPath(id), normalized)
}

func peakEntryWithLegacyWindow(existing *config.ProviderPeakHours, window *config.PeakHoursWindow) *config.ProviderPeakHours {
	if existing != nil && existing.Policy != nil {
		policy := existing.AsPolicy()
		policy.Windows = []config.PeakHoursWindowSpec{{Start: window.Start, End: window.End}}
		policy.Message = window.Message
		return &config.ProviderPeakHours{Policy: policy}
	}
	return &config.ProviderPeakHours{Legacy: window}
}
