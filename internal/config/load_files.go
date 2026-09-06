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

	"github.com/PHPCraftdream/rush/internal/fsext"
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

// lookupConfigCandidates returns every fixed and bounded project path that
// lookupConfigs can inspect, including paths that do not exist yet. Stale
// tracking must retain these negative lookups so creating a previously
// absent config file is observable by the watcher.
func lookupConfigCandidates(cwd string) []string {
	candidates := lookupConfigCandidateGroups(cwd)
	return append(candidates.global, candidates.project...)
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
		{systemConfigPath, systemConfigOwner()},
		{GlobalConfig(), homeConfigOwner()},
		{GlobalConfigData(), homeConfigOwner()},
	} {
		if eligible := eligibleConfigCandidate(candidate.path, candidate.owner); eligible != "" {
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
		for _, name := range []string{appName + ".json", "." + appName + ".json"} {
			if candidate := eligibleConfigCandidate(filepath.Join(dir, name), owner); candidate != "" {
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

// configOwnerForWorkingDir returns the explicit owner policy used when a
// discovered file is opened. Discovery's path check is only a filter; this
// second check closes the stat/open race.
func configOwnerForWorkingDir(workingDir string) (int, bool, error) {
	if workingDir == "" {
		return 0, false, nil
	}
	owner, err := fsext.Owner(canonicalConfigPath(workingDir))
	if err != nil {
		return 0, false, err
	}
	return owner, true, nil
}

func configOwnerPolicyForPath(path, workingDir string) (int, bool, error) {
	if path == "" {
		return 0, false, nil
	}
	canonical := normalizeReloadPath(path)
	if systemConfigPath != "" && canonical == normalizeReloadPath(systemConfigPath) {
		return systemConfigOwner(), true, nil
	}
	if canonical == normalizeReloadPath(GlobalConfig()) || canonical == normalizeReloadPath(GlobalConfigData()) {
		return homeConfigOwner(), true, nil
	}
	return configOwnerForWorkingDir(workingDir)
}

// canonicalConfigPath resolves the starting directory before both the git
// boundary probe and the upward walk. A symlinked working directory can point
// into a nested repository; walking its lexical parents would otherwise let
// the search escape that repository and adopt an unrelated parent config.
func canonicalConfigPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	} else if !os.IsNotExist(err) {
		return abs
	}

	// EvalSymlinks cannot resolve an absent leaf, but its existing parent may
	// still be a symlink or junction. Resolve that parent so an absent
	// candidate and the same candidate reached through an alias share the
	// identity used by reload fingerprints.
	var missing []string
	for current := abs; ; current = filepath.Dir(current) {
		missing = append(missing, filepath.Base(current))
		parent := filepath.Dir(current)
		resolved, resolveErr := filepath.EvalSymlinks(parent)
		if resolveErr == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved)
		}
		if parent == current || !os.IsNotExist(resolveErr) {
			return abs
		}
	}
}

func configOwnersMatch(expected, actual int) bool {
	return expected == -1 || actual == expected
}

// eligibleWorkspaceConfig returns the workspace config only when the
// working directory has a trustworthy owner and the config has that same
// owner. A workspace config is project-scoped even when data_directory points
// outside the checkout, so it uses the checkout policy rather than the home
// or system policy.
func eligibleWorkspaceConfig(path, workingDir string) string {
	if workingDir == "" {
		return ""
	}
	owner, err := fsext.Owner(canonicalConfigPath(workingDir))
	if err != nil {
		return ""
	}
	return eligibleConfigCandidate(path, owner)
}

// eligibleConfigCandidate retains absent candidates for negative staleness
// tracking, while rejecting existing foreign-owned files and directories.
func eligibleConfigCandidate(path string, owner int) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return path
	}
	if err != nil || info.IsDir() {
		return ""
	}
	candidateOwner, err := fsext.Owner(path)
	if err != nil || !configOwnersMatch(owner, candidateOwner) {
		return ""
	}
	return path
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
	clean := normalizeReloadPath(path)
	return slices.ContainsFunc(loadedPaths, func(p string) bool {
		return normalizeReloadPath(p) == clean
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
	return loadConfigCandidateStableForWorkingDir(configPaths, "")
}

func loadConfigCandidateStableForWorkingDir(configPaths []string, workingDir string) (*Config, []string, map[string]reloadFileFingerprint, []stableConfigDocument, error) {
	documents, err := readStableConfigDocumentsWithOwner(configPaths, func(path string) (int, bool, error) {
		return configOwnerPolicyForPath(path, workingDir)
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, document := range documents {
		if document.present && len(document.data) > 0 && !json.Valid(document.data) {
			return nil, nil, nil, nil, fmt.Errorf("invalid JSON in config file %s", document.path)
		}
		if document.present {
			if err := validateMCPDisabledOverlays(document.data); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("invalid MCP configuration in config file %s: %w", document.path, err)
			}
		}
	}
	configs, loaded, fingerprints := configDocumentBytes(documents)
	addDocumentAliasFingerprints(fingerprints, documents)

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
	aliases     []stableConfigAlias
}

type stableConfigAlias struct {
	path        string
	fingerprint reloadFileFingerprint
}

func readStableConfigDocuments(paths []string) ([]stableConfigDocument, error) {
	return readStableConfigDocumentsWithOwner(paths, nil)
}

func readStableConfigDocumentsWithOwner(paths []string, owner func(string) (int, bool, error)) ([]stableConfigDocument, error) {
	documents := make([]stableConfigDocument, 0, len(paths))
	seen := make(map[string]int, len(paths))
	for _, path := range paths {
		discoveryPath := normalizeDiscoveryPath(path)
		if discoveryPath == "" {
			continue
		}
		canonicalPath := normalizeReloadPath(discoveryPath)
		expectedOwner, enforceOwner := 0, false
		var ownerErr error
		if owner != nil {
			expectedOwner, enforceOwner, ownerErr = owner(discoveryPath)
			if ownerErr != nil {
				return nil, ownerErr
			}
		}
		data, fingerprint, readErr := readStableConfigFileOwned(discoveryPath, expectedOwner, enforceOwner)
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, fmt.Errorf("failed to open config file %s: %w", discoveryPath, readErr)
		}
		alias := stableConfigAlias{path: discoveryPath, fingerprint: fingerprint}
		if index, ok := seen[canonicalPath]; ok {
			documents[index].aliases = append(documents[index].aliases, alias)
			continue
		}
		seen[canonicalPath] = len(documents)
		if readErr != nil {
			documents = append(documents, stableConfigDocument{path: discoveryPath, aliases: []stableConfigAlias{alias}})
			continue
		}
		documents = append(documents, stableConfigDocument{
			path: discoveryPath, data: data, fingerprint: fingerprint, present: fingerprint.exists,
			aliases: []stableConfigAlias{alias},
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
