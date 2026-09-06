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

	"github.com/qjebbs/go-jsons"
)

// lookupConfigs searches config files starting at cwd and walking up
// through the current project. The upward walk stops at the git
// working tree root when one can be detected, otherwise at cwd itself,
// so an unrelated rush.json placed above the project is never picked
// up. Global user-level config locations are always included
// regardless of the boundary.
func lookupConfigs(cwd string) []string {
	paths := lookupConfigCandidates(cwd)
	result := slices.Clone(paths[:min(3, len(paths))])
	for _, path := range paths[min(3, len(paths)):] {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			result = append(result, path)
		}
	}
	return result
}

// lookupConfigCandidates returns every fixed and bounded project path that
// lookupConfigs can inspect, including paths that do not exist yet. Stale
// tracking must retain these negative lookups so creating a previously
// absent config file is observable by the watcher.
func lookupConfigCandidates(cwd string) []string {
	paths := []string{systemConfigPath, GlobalConfig(), GlobalConfigData()}
	if cwd == "" {
		return paths
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = filepath.Clean(cwd)
	}
	boundary := projectBoundary(cwd)
	boundary, err = filepath.Abs(boundary)
	if err != nil {
		boundary = filepath.Clean(boundary)
	}
	var projectPaths []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		projectPaths = append(projectPaths,
			filepath.Join(dir, appName+".json"),
			filepath.Join(dir, "."+appName+".json"),
		)
		if sameDir(dir, boundary) || filepath.Dir(dir) == dir {
			break
		}
	}
	// The merge contract is lowest priority first. The upward walk discovers
	// the nearest directory first, so reverse the complete candidate list as
	// well as the historical existing-file discovery result.
	slices.Reverse(projectPaths)
	paths = append(paths, projectPaths...)
	return paths
}

// pathAlreadyLoaded reports whether path (typically the computed workspace
// config path) is already present in loadedPaths (the paths loadFromConfigPaths
// already merged into cfg).
//
// Load and buildAndPublishReload both merge the workspace config
// (<DataDirectory>/rush.json) as a SEPARATE step after loadFromConfigPaths,
// under the assumption that it's a distinct file loadFromConfigPaths didn't
// already see. That assumption breaks when DataDirectory is configured (or,
// as in several tests, passed directly) such that the workspace path
// resolves to a path lookupConfigs already discovered and loaded — the file
// then gets read and merged a SECOND time via mustMarshalConfig(cfg)+wsData.
// jsons.Merge appends JSON arrays instead of overriding them (see
// internal/merge/ordered.go in github.com/qjebbs/go-jsons), so re-merging
// the same provider's "models" array against itself duplicates it, while
// scalar fields like models.smart.model are simply overwritten by the
// second read — the two reads of the same file are not even guaranteed to
// observe the same content if a concurrent writer lands between them,
// producing a config whose Models selection and whose Providers model list
// disagree (task #458). Skipping the second read/merge whenever the
// workspace path was already loaded closes this both structurally (no more
// double-processing of one file's content) and for the concurrent-write
// case (no second, possibly-different read of the same path).
func pathAlreadyLoaded(loadedPaths []string, path string) bool {
	clean := filepath.Clean(path)
	return slices.ContainsFunc(loadedPaths, func(p string) bool {
		return filepath.Clean(p) == clean
	})
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
	documents, err := readStableConfigDocuments(configPaths)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, document := range documents {
		if document.present && len(document.data) > 0 && !json.Valid(document.data) {
			return nil, nil, nil, nil, fmt.Errorf("invalid JSON in config file %s", document.path)
		}
	}
	configs, loaded, fingerprints := configDocumentBytes(documents)

	cfg, err := loadFromBytes(configs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cfg, loaded, fingerprints, documents, nil
}

// stableConfigDocument is the exact byte document used to build a candidate.
// Keeping the bytes and fingerprint together prevents a second read from
// supplying overlay metadata that belongs to a different generation.
type stableConfigDocument struct {
	path        string
	data        []byte
	fingerprint reloadFileFingerprint
	present     bool
}

func readStableConfigDocuments(paths []string) ([]stableConfigDocument, error) {
	documents := make([]stableConfigDocument, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = normalizeReloadPath(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		data, fingerprint, err := readStableConfigFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				documents = append(documents, stableConfigDocument{path: path})
				continue
			}
			return nil, fmt.Errorf("failed to open config file %s: %w", path, err)
		}
		documents = append(documents, stableConfigDocument{
			path: path, data: data, fingerprint: fingerprint, present: fingerprint.exists,
		})
	}
	return documents, nil
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
		if len(document.data) == 0 {
			continue
		}
		configs = append(configs, document.data)
		loaded = append(loaded, document.path)
	}
	return configs, loaded, fingerprints
}

func loadFromBytes(configs [][]byte) (*Config, error) {
	if len(configs) == 0 {
		return &Config{}, nil
	}

	configs = sanitizeMCPDisabledOverlays(configs)
	data, err := jsons.Merge(configs)
	if err != nil {
		return nil, err
	}
	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	warnUnknownModelSlots(data)
	return &config, nil
}

// sanitizeMCPDisabledOverlays removes the special one-field MCP entries
// before the generic deep merge runs. A disabled-only entry is an overlay for
// an external .mcp.json server; treating it as an ordinary MCP definition
// would incorrectly toggle a complete definition from another Rush scope.
func sanitizeMCPDisabledOverlays(configs [][]byte) [][]byte {
	cleaned := make([][]byte, 0, len(configs))
	for _, data := range configs {
		var root map[string]json.RawMessage
		if json.Unmarshal(data, &root) != nil || root == nil {
			cleaned = append(cleaned, data)
			continue
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
	return cleaned
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
