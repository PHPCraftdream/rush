package provpeak

import (
	"strings"

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

// EscapeSJSONPathComponent escapes a provider ID or field for an SJSON path.
func EscapeSJSONPathComponent(s string) string {
	return strings.NewReplacer(`\`, `\\`, ".", `\.`, "*", `\*`, "?", `\?`, "#", `\#`, "|", `\|`, "@", `\@`).Replace(s)
}
