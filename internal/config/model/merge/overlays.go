// MCP disabled-overlay sanitation and the unknown-model-slot diagnostic,
// moved verbatim from the root load_files.go in D2-1. The two entry
// predicates are exported because staying-root store code calls them
// through unexported root wrappers.
package cfgmerge

import (
	"encoding/json"
	"fmt"
	"log/slog"

	cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"
)

// sanitizeMCPDisabledOverlays removes the special one-field MCP entries
// before the generic deep merge runs. A disabled-only entry is an overlay for
// an external .mcp.json server; treating it as an ordinary MCP definition
// would incorrectly toggle a complete definition from another Rush scope.
func sanitizeMCPDisabledOverlays(configs [][]byte) ([][]byte, error) {
	cleaned := make([][]byte, 0, len(configs))
	for _, data := range configs {
		var root map[string]json.RawMessage
		if json.Unmarshal(data, &root) != nil || root == nil {
			cleaned = append(cleaned, data)
			continue
		}
		if err := ValidateMCPDisabledOverlays(data); err != nil {
			return nil, err
		}
		var mcp map[string]json.RawMessage
		if raw, ok := root["mcp"]; ok && json.Unmarshal(raw, &mcp) == nil && mcp != nil {
			for name, rawEntry := range mcp {
				var entry map[string]json.RawMessage
				if json.Unmarshal(rawEntry, &entry) == nil && isMCPDisabledOnlyEntry(entry, true) {
					delete(mcp, name)
				}
			}
			encodedMCP, err := json.Marshal(mcp)
			if err == nil {
				root["mcp"] = encodedMCP
			}
		}
		encoded, err := json.Marshal(root)
		if err != nil {
			cleaned = append(cleaned, data)
			continue
		}
		cleaned = append(cleaned, encoded)
	}
	return cleaned, nil
}

func ValidateMCPDisabledOverlays(data []byte) error {
	var root struct {
		MCP map[string]json.RawMessage `json:"mcp"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	for name, rawEntry := range root.MCP {
		var entry map[string]json.RawMessage
		if json.Unmarshal(rawEntry, &entry) != nil || !isMCPDisabledOnlyEntry(entry, true) {
			continue
		}
		var disabled any
		if err := json.Unmarshal(entry["disabled"], &disabled); err != nil {
			return fmt.Errorf("invalid MCP disabled override for %q: disabled must be a boolean", name)
		}
		if _, ok := disabled.(bool); !ok {
			return fmt.Errorf("invalid MCP disabled override for %q: disabled must be a boolean", name)
		}
	}
	return nil
}

// knownModelSlots is the exhaustive set of keys the "models" object in
// rush.json is read into (see the SelectedModelType* constants). Anything
// else under "models" is not a typo Go will catch: json.Unmarshal into
// Config.Models (map[SelectedModelType]SelectedModel) silently keeps an
// unrecognized key as a map entry that is simply never looked up anywhere
// -- Load never errors, "rush models state" just reports whatever the
// defaults resolve to, and the operator has no signal that the key they
// wrote did nothing. This bites hardest after a model-slot rename: a config
// file still holding a pre-rename slot key under "models" loads
// "successfully" and silently runs different models than the file claims.
// This function is a diagnostic ONLY -- it must never change
// which model gets selected, only warn when a written key cannot possibly
// affect that selection.
var knownModelSlots = map[string]bool{
	string(cfgmodel.SelectedModelTypeSmart):    true,
	string(cfgmodel.SelectedModelTypeFast):     true,
	string(cfgmodel.SelectedModelTypeWorker):   true,
	string(cfgmodel.SelectedModelTypeReviewer): true,
}

// isMCPDisabledOnlyEntry is a byte-identical local copy of the root
// package unexported helper (D2-1 boundary closure): the function is
// defined in the staying-root mcpdocs.go, and the moved overlay
// sanitation needs it. It is a small pure predicate.
func isMCPDisabledOnlyEntry(entry map[string]json.RawMessage, exists bool) bool {
	if !exists || len(entry) != 1 {
		return false
	}
	_, hasDisabled := entry["disabled"]
	return hasDisabled
}

// warnUnknownModelSlots inspects the raw (pre-unmarshal) "models" object for
// keys outside knownModelSlots and logs one warning per offender. It reads
// the merged JSON bytes directly rather than the typed Config, because by
// the time the data reaches a map[SelectedModelType]SelectedModel field the
// unrecognized key has already been accepted as a valid (if unused) map
// entry -- there is nothing left in the typed value to detect the mistake
// from. Malformed "models" JSON is silently ignored here: loadFromBytes's
// own json.Unmarshal into Config already surfaces a real parse error to the
// caller, so this best-effort peek doesn't need to duplicate that.
func warnUnknownModelSlots(data []byte) {
	var probe struct {
		Models map[string]json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return
	}
	for key := range probe.Models {
		if !knownModelSlots[key] {
			slog.Warn("unrecognized key under \"models\" in rush.json -- it is not one of the configured model slots and has no effect", "key", key, "known_slots", "smart, fast, worker, reviewer")
		}
	}
}
