// Config-file candidate discovery: which rush.json files are eligible under the
// owner policy and which of them were already loaded.
package docread

import (
	"os"
	"path/filepath"
	"slices"

	"github.com/PHPCraftdream/rush/internal/config/domains/datadir"
	"github.com/PHPCraftdream/rush/internal/config/io/fileid"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
)

// lookupConfigCandidates returns every fixed and bounded project path that
// lookupConfigs can inspect, including paths that do not exist yet. Stale
// tracking must retain these negative lookups so creating a previously
// absent config file is observable by the watcher.
func lookupConfigCandidates(cwd string) []string {
	candidates := LookupConfigCandidateGroups(cwd)
	return append(candidates.Global, candidates.Project...)
}

type ConfigCandidateGroups struct {
	Global  []string
	Project []string
}

func LookupConfigCandidateGroups(cwd string) ConfigCandidateGroups {
	candidates := ConfigCandidateGroups{Global: make([]string, 0, 3)}
	for _, candidate := range []struct {
		path  string
		owner int
	}{
		{datadir.SystemConfig(), fileid.SystemConfigOwner()},
		{datadir.GlobalConfig(), fileid.HomeConfigOwner()},
		{datadir.GlobalConfigData(), fileid.HomeConfigOwner()},
	} {
		// Each logical slot resolves to exactly one file: rush.ktav when it
		// exists, otherwise the legacy rush.json spelling.
		if eligible := eligibleConfigCandidate(selectConfigSlot(candidate.path), candidate.owner); eligible != "" {
			candidates.Global = append(candidates.Global, eligible)
		}
	}
	if cwd == "" {
		return candidates
	}
	abs := datadir.CanonicalConfigPath(cwd)
	boundary := datadir.CanonicalConfigPath(datadir.ProjectBoundary(abs))
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
		if datadir.SameDir(dir, boundary) || filepath.Dir(dir) == dir {
			break
		}
	}
	// The merge contract is lowest priority first. The upward walk discovers
	// the nearest directory first, so reverse the complete candidate list as
	// well as the historical existing-file discovery result.
	slices.Reverse(projectPaths)
	candidates.Project = projectPaths
	return candidates
}

func configOwnersMatch(expected, actual int) bool {
	return expected == -1 || actual == expected
}

// eligibleConfigCandidate retains absent candidates for negative staleness
// tracking, while rejecting existing foreign-owned files and directories.
func eligibleConfigCandidate(path string, owner int) string {
	if path == "" {
		return ""
	}
	isKtav := isKtavConfigPath(path)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return path
	}
	if err != nil || info.IsDir() {
		if isKtav {
			return path
		}
		return ""
	}
	candidateOwner, err := fsext.Owner(path)
	if err != nil {
		if isKtav {
			return path
		}
		return ""
	}
	if !configOwnersMatch(owner, candidateOwner) {
		return ""
	}
	return path
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
	owner, err := fsext.Owner(datadir.CanonicalConfigPath(workingDir))
	if err != nil {
		return ""
	}
	return eligibleConfigCandidate(path, owner)
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

// configOwnerForWorkingDir returns the explicit owner policy used when a
// discovered file is opened. Discovery's path check is only a filter; this
// second check closes the stat/open race.
func configOwnerForWorkingDir(workingDir string) (int, bool, error) {
	if workingDir == "" {
		return 0, false, nil
	}
	owner, err := fsext.Owner(datadir.CanonicalConfigPath(workingDir))
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
	if datadir.SystemConfigSlot() != "" && matchesLogicalSlot(canonical, datadir.SystemConfigSlot()) {
		return fileid.SystemConfigOwner(), true, nil
	}
	if matchesLogicalSlot(canonical, datadir.GlobalConfig()) || matchesLogicalSlot(canonical, datadir.GlobalConfigData()) {
		return fileid.HomeConfigOwner(), true, nil
	}
	return configOwnerForWorkingDir(workingDir)
}
