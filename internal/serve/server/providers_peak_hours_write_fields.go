package server

import "github.com/PHPCraftdream/rush/internal/config"

// SetConfigFields applies the parent key before its sorted child override.
func customProviderWriteFields(field string, provider config.ProviderConfig, scope config.Scope) map[string]any {
	fields := map[string]any{field: provider}
	if scope == config.ScopeWorkspace && provider.PeakHours == nil {
		fields[field+".peak_hours"] = nil
	}
	return fields
}
