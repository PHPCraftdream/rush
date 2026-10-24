// Slot-aware physical targets for MCP transactions: every point where a
// stored logical scope path (rush.json) becomes the file an MCP transaction
// really locks, reads, and rewrites routes through ONE helper,
// mcpConfigSlotPath, so lock targets, binding verification, identity
// expectations, and the commit itself all bind the SAME physical file.
package config

import (
	"path/filepath"
	"strings"
)

// mcpConfigSlotPath resolves the physical write target of an MCP transaction
// for a stored scope or candidate path:
//
//   - external .mcp.json paths are returned unchanged: that document is not
//     a Rush slot and must never be slot-selected or backup-checked;
//   - every other path is normalized to its logical LEGACY spelling
//     (legacySlotOf), fails closed on a backup-only slot
//     (checkBackupOnlySlot — that error is a hard ERROR, never "absent"),
//     and is resolved to the active slot with writeTargetSlot (the rush.ktav
//     sibling wins when it exists; when neither format exists the
//     ktavAutoMigrate gate decides, false today -> rush.json).
//
// The helper is idempotent: an already-selected .ktav path maps back to its
// legacy spelling and reselects the same physical file.
func mcpConfigSlotPath(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if strings.EqualFold(filepath.Base(path), ".mcp.json") {
		return path, nil
	}
	legacy := legacySlotOf(normalizeReloadPath(path))
	if err := checkBackupOnlySlot(legacy); err != nil {
		return "", err
	}
	return writeTargetSlot(legacy), nil
}
