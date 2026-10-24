// Ktav-capable provider/model-catalog cache layer: Rush-owned LOCAL cache
// files (providers.json, hyper.json, model-catalog-<id>.json) follow the
// same two-generation naming as config documents — an active <name>.ktav
// sibling and a retained <name>.json original. Remote Catwalk/Hyper
// protocol bodies stay JSON in both directions.
package config

import (
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
)

// readCacheDocument reads the active generation of a Rush-owned cache slot
// named by its LEGACY JSON path and returns JSON-shaped bytes. A present
// .ktav sibling always wins: it is decoded through ktavcodec and its
// JSON-shaped bytes are returned, and a coexisting legacy .json file is
// reported once per file with a paths-only warning. An invalid Ktav file is
// an error for the slot — it must never silently fall back to the stale
// JSON copy. When only the retained <name>.json.bak backup exists, the
// backup is never restored: the read reports exactly like a missing cache
// file. When neither generation exists the legacy path's not-exist error is
// returned unchanged.
func readCacheDocument(legacyJSON string) ([]byte, error) {
	ktav := ktavSiblingOf(legacyJSON)
	if _, statErr := os.Stat(ktav); statErr == nil || !ktavEntryAbsentErr(statErr) {
		raw, err := os.ReadFile(ktav)
		if err != nil {
			return nil, err
		}
		tree, err := ktavcodec.DecodeKtav(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid Ktav in provider cache file %s: %w", ktav, err)
		}
		jsonBytes, err := ktavcodec.ToJSON(tree)
		if err != nil {
			return nil, fmt.Errorf("invalid Ktav in provider cache file %s: %w", ktav, err)
		}
		if _, jsonErr := os.Stat(legacyJSON); jsonErr == nil {
			warnBothFormatsOnce(ktav, legacyJSON)
		}
		return jsonBytes, nil
	}
	return os.ReadFile(legacyJSON)
}

// writeCacheDocument writes JSON-shaped cache bytes in the gate-selected
// format. With the gate off the legacy JSON path and os.WriteFile bytes are
// unchanged from before Ktav existed. With the gate on the bytes are
// converted strictly to Ktav and the <name>.ktav sibling is published
// through the same atomic stage+rename helper the config documents use; the
// legacy JSON file is deliberately left alone because reads always prefer
// the Ktav sibling, so a stale JSON copy can never win.
func writeCacheDocument(legacyJSON string, jsonBytes []byte, perm os.FileMode) error {
	if !newConfigFileIsKtav() {
		return os.WriteFile(legacyJSON, jsonBytes, perm)
	}
	ktavBytes, err := ktavcodec.ConvertJSONToKtav(jsonBytes)
	if err != nil {
		return fmt.Errorf("failed to convert provider cache to Ktav: %w", err)
	}
	return atomicWriteFile(ktavSiblingOf(legacyJSON), ktavBytes, perm)
}
