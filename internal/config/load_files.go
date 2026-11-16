// Config-file discovery and merging: the bounded upward search for
// rush.json files, duplicate-workspace-config detection, and the JSON
// merge that folds every loaded document into one Config.
package config

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
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
