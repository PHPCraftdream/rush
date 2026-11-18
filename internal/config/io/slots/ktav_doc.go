// Format-aware settings document write-target selection.
package slots

import "github.com/PHPCraftdream/rush/internal/config/io/docread"

// NewConfigFileIsKtav reports whether a new config file should use Ktav.
func NewConfigFileIsKtav() bool { return newConfigFileIsKtav() }

// WriteTargetSlot resolves the physical write target for a legacy config path.
func WriteTargetSlot(legacyPath string) string { return writeTargetSlot(legacyPath) }

// newConfigFileIsKtav reports whether a new config file should use Ktav.
func newConfigFileIsKtav() bool { return ktavAutoMigrate }

// writeTargetSlot resolves the physical write target for a legacy config path.
func writeTargetSlot(legacyPath string) string {
	if legacyPath == "" || docread.IsKtavConfigPath(legacyPath) {
		return legacyPath
	}
	selected := docread.SelectConfigSlot(legacyPath)
	if selected == legacyPath && newConfigFileIsKtav() && !docread.KtavEntryExists(legacyPath) {
		return docread.KtavSiblingOf(legacyPath)
	}
	return selected
}
