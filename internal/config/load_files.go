// Config-file discovery and merging: the bounded upward search for
// rush.json files, duplicate-workspace-config detection, and the JSON
// merge that folds every loaded document into one Config.
package config

import (
	"os"
	"slices"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

// lookupConfigs searches config files starting at cwd and walking up
// through the current project. The upward walk stops at the git
// working tree root when one can be detected, otherwise at cwd itself,
// so an unrelated rush.json placed above the project is never picked
// up. Global user-level config locations are always included
// regardless of the boundary.
func lookupConfigs(cwd string) []string {
	candidates := docread.LookupConfigCandidateGroups(cwd)
	// Keep global provenance separate from project provenance. The old
	// first-three rule silently classified a project file as global whenever a
	// global candidate was rejected by the owner policy.
	result := slices.Clone(candidates.Global)
	for _, path := range candidates.Project {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			result = append(result, path)
		}
	}
	return result
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
	cfg, loaded, fingerprints, _, err := loadConfigCandidateStableForWorkingDir(configPaths, "")
	return cfg, loaded, fingerprints, err
}

func loadConfigCandidateStable(configPaths []string) (*Config, []string, map[string]reloadFileFingerprint, []stableConfigDocument, error) {
	return loadConfigCandidateStableForWorkingDir(configPaths, "")
}
