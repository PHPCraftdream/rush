// Config-file discovery and merging: the bounded upward search for
// rush.json files, duplicate-workspace-config detection, and the JSON
// merge that folds every loaded document into one Config.
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
	"github.com/qjebbs/go-jsons"
)

// lookupConfigs searches config files starting at cwd and walking up
// through the current project. The upward walk stops at the git
// working tree root when one can be detected, otherwise at cwd itself,
// so an unrelated rush.json placed above the project is never picked
// up. Global user-level config locations are always included
// regardless of the boundary.
func lookupConfigs(cwd string) []string {
	candidates := lookupConfigCandidateGroups(cwd)
	// Keep global provenance separate from project provenance. The old
	// first-three rule silently classified a project file as global whenever a
	// global candidate was rejected by the owner policy.
	result := slices.Clone(candidates.global)
	for _, path := range candidates.project {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			result = append(result, path)
		}
	}
	return result
}

type configCandidateGroups struct {
	global  []string
	project []string
}

func lookupConfigCandidateGroups(cwd string) configCandidateGroups {
	candidates := configCandidateGroups{global: make([]string, 0, 3)}
	for _, candidate := range []struct {
		path  string
		owner int
	}{
		{systemConfigSlot(), systemConfigOwner()},
		{GlobalConfig(), homeConfigOwner()},
		{GlobalConfigData(), homeConfigOwner()},
	} {
		// Each logical slot resolves to exactly one file: rush.ktav when it
		// exists, otherwise the legacy rush.json spelling.
		if eligible := eligibleConfigCandidate(selectConfigSlot(candidate.path), candidate.owner); eligible != "" {
			candidates.global = append(candidates.global, eligible)
		}
	}
	if cwd == "" {
		return candidates
	}
	abs := canonicalConfigPath(cwd)
	boundary := canonicalConfigPath(projectBoundary(abs))
	owner, err := fsext.Owner(abs)
	if err != nil {
		return candidates
	}
	var projectPaths []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		dirOwner, err := fsext.Owner(dir)
		if err != nil || !configOwnersMatch(owner, dirOwner) {
			break
		}
		for _, base := range configSlotBaseNames() {
			// Visible rush.ktav/rush.json keeps its historical priority over
			// the hidden pair; only one format per slot is ever selected.
			selected := selectConfigSlot(filepath.Join(dir, base+".json"))
			if candidate := eligibleConfigCandidate(selected, owner); candidate != "" {
				projectPaths = append(projectPaths, candidate)
			}
		}
		if sameDir(dir, boundary) || filepath.Dir(dir) == dir {
			break
		}
	}
	// The merge contract is lowest priority first. The upward walk discovers
	// the nearest directory first, so reverse the complete candidate list as
	// well as the historical existing-file discovery result.
	slices.Reverse(projectPaths)
	candidates.project = projectPaths
	return candidates
}

func configOwnersMatch(expected, actual int) bool {
	return expected == -1 || actual == expected
}

func loadFromConfigPaths(configPaths []string) (*Config, []string, error) {
	cfg, loaded, _, err := loadFromConfigPathsStable(configPaths)
	return cfg, loaded, err
}

// loadFromConfigPathsStable loads each path from a bounded stable byte read.
// The returned fingerprints describe the exact bytes supplied to the JSON
// merge, rather than a separate stat taken before the read. That distinction
// is what rejects an ABA edit (A -> B -> A) when B was the candidate parsed.
func loadFromConfigPathsStable(configPaths []string) (*Config, []string, map[string]reloadFileFingerprint, error) {
	cfg, loaded, fingerprints, _, err := loadConfigCandidateStable(configPaths)
	return cfg, loaded, fingerprints, err
}

func loadConfigCandidateStable(configPaths []string) (*Config, []string, map[string]reloadFileFingerprint, []stableConfigDocument, error) {
	return loadConfigCandidateStableForWorkingDir(configPaths, "")
}

func readStableConfigDocuments(paths []string) ([]stableConfigDocument, error) {
	return readStableConfigDocumentsWithOwner(paths, nil)
}

func configDocumentBytes(documents []stableConfigDocument) ([][]byte, []string, map[string]reloadFileFingerprint) {
	configs := make([][]byte, 0, len(documents))
	loaded := make([]string, 0, len(documents))
	fingerprints := make(map[string]reloadFileFingerprint, len(documents))
	for _, document := range documents {
		fingerprints[document.path] = document.fingerprint
		if !document.present {
			continue
		}
		loaded = append(loaded, document.path)
		if len(document.data) > 0 {
			configs = append(configs, document.data)
		}
	}
	return configs, loaded, fingerprints
}

func addDocumentAliasFingerprints(fingerprints map[string]reloadFileFingerprint, documents []stableConfigDocument) {
	for _, document := range documents {
		for _, alias := range document.aliases {
			fingerprints[alias.path] = alias.fingerprint
		}
	}
}

func loadFromBytes(configs [][]byte) (*Config, error) {
	if len(configs) == 0 {
		return &Config{}, nil
	}

	var err error
	configs, err = sanitizeMCPDisabledOverlays(configs)
	if err != nil {
		return nil, err
	}
	configs, roles, err := stripCustomRoleOverlays(configs)
	if err != nil {
		return nil, err
	}
	// Peak-hours scopes are resolved outside the generic merge: the
	// weekly policy form must override between scopes entirely, and an
	// incomplete legacy fragment over a policy is a hard error.
	stripped, peakOverlays, err := stripPeakHoursOverlays(configs)
	if err != nil {
		return nil, err
	}
	resolvedPeakHours := make(map[string]json.RawMessage, len(peakOverlays))
	for id, providerOverlays := range peakOverlays {
		winner, err := resolvePeakHoursOverlays(id, providerOverlays)
		if err != nil {
			return nil, err
		}
		resolvedPeakHours[id] = winner
	}
	data, err := jsons.Merge(stripped)
	if err != nil {
		return nil, err
	}
	data, err = applyPeakHoursOverlays(data, resolvedPeakHours)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	if len(roles) != 0 {
		config.Roles = roles
	}
	warnUnknownModelSlots(data)
	return &config, nil
}

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
		if err := validateMCPDisabledOverlays(data); err != nil {
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

func validateMCPDisabledOverlays(data []byte) error {
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
	string(SelectedModelTypeSmart):    true,
	string(SelectedModelTypeFast):     true,
	string(SelectedModelTypeWorker):   true,
	string(SelectedModelTypeReviewer): true,
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
