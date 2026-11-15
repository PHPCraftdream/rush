// Compiler-forced boundary exports (D2-4 cluster 1, datadir).
//
// The moved bodies above keep their unexported names byte-identical;
// staying-root package `config` code and tests still reference those
// names, which are invisible across the package boundary. Each wrapper
// below is a one-line forwarder added per the boundary rule (≤15 local
// copies allowed; this cluster uses ZERO copies — every need is closed
// with an exported forwarder here plus a facade forwarder in the root
// package). Mutable state is never exported directly: workspaceHomeFlag
// and worktreeRootCache are reached only through these functions.
package datadir

import "os/exec"

// ResolveDataDirectory forwards resolveDataDirectory.
func ResolveDataDirectory(workingDir, dataDirFlag, dataDirFromConfig string) (string, DataDirSource) {
	return resolveDataDirectory(workingDir, dataDirFlag, dataDirFromConfig)
}

// DataDirIsSharedOfLinkedWorktree forwards dataDirIsSharedOfLinkedWorktree.
func DataDirIsSharedOfLinkedWorktree(workingDir, dataDir string, source DataDirSource) bool {
	return dataDirIsSharedOfLinkedWorktree(workingDir, dataDir, source)
}

// DefaultDataDirForWorkspace forwards defaultDataDirForWorkspace.
func DefaultDataDirForWorkspace(workingDir string) (string, DataDirSource) {
	return defaultDataDirForWorkspace(workingDir)
}

// IsDevBuild forwards isDevBuild.
func IsDevBuild() bool { return isDevBuild() }

// WorktreeRoot forwards worktreeRoot (the unexported cached probe).
func WorktreeRoot(dir string) string { return worktreeRoot(dir) }

// ProjectBoundary forwards projectBoundary.
func ProjectBoundary(dir string) string { return projectBoundary(dir) }

// CanonicalConfigPath forwards canonicalConfigPath.
func CanonicalConfigPath(path string) string { return canonicalConfigPath(path) }

// SameDir forwards sameDir.
func SameDir(a, b string) bool { return sameDir(a, b) }

// SystemConfigSlot forwards systemConfigSlot.
func SystemConfigSlot() string { return systemConfigSlot() }

// CachePathFor forwards cachePathFor.
func CachePathFor(name string) string { return cachePathFor(name) }

// SystemConfigPath re-exports the GOOS-selected systemConfigPath const.
const SystemConfigPath = systemConfigPath

// SetWorkspaceHome stores the process home flag (workspaceHomeFlag) for
// staying-root load.go, which previously wrote the variable directly.
func SetWorkspaceHome(v bool) { workspaceHomeFlag.Store(v) }

// SetDataDirHomeForTest forwards setDataDirHomeForTest.
func SetDataDirHomeForTest(v bool) { setDataDirHomeForTest(v) }

// SetOSExecutablePathForTest forwards setOSExecutablePathForTest.
func SetOSExecutablePathForTest(path string) func() { return setOSExecutablePathForTest(path) }

// SetDataDirRedirectionOverride forwards setDataDirRedirectionOverride.
func SetDataDirRedirectionOverride(v int) func() { return setDataDirRedirectionOverride(v) }

// SharedMarkerExists forwards sharedMarkerExists.
func SharedMarkerExists(mainDataDir, wsRoot string) bool {
	return sharedMarkerExists(mainDataDir, wsRoot)
}

// RedirectionAllowed forwards redirectionAllowed.
func RedirectionAllowed() bool { return redirectionAllowed() }

// WorktreeRootCacheStore stores a seam entry in worktreeRootCache for
// staying-root discovery tests.
func WorktreeRootCacheStore(key, value string) { worktreeRootCache.Store(key, value) }

// WorktreeRootCacheDelete removes a seam entry from worktreeRootCache.
func WorktreeRootCacheDelete(key string) { worktreeRootCache.Delete(key) }

// WorktreeRootCacheLookup loads a worktreeRootCache entry.
func WorktreeRootCacheLookup(key string) (string, bool) {
	value, ok := worktreeRootCache.Load(key)
	if !ok {
		return "", false
	}
	root, _ := value.(string)
	return root, true
}

// DetectDevBuild forwards detectDevBuild (white-box test seam).
func DetectDevBuild() bool { return detectDevBuild() }

// LinkedWorktreeInfo forwards linkedWorktreeInfo (white-box test seam).
func LinkedWorktreeInfo(wtRoot string) *linkedWorktree { return linkedWorktreeInfo(wtRoot) }

// ProbeLinkedWorktree forwards probeLinkedWorktree (white-box test seam).
func ProbeLinkedWorktree(wtRoot string) *linkedWorktree { return probeLinkedWorktree(wtRoot) }

// StripGitWorktreeEnv forwards stripGitWorktreeEnv (white-box test seam).
func StripGitWorktreeEnv(cmd *exec.Cmd) { stripGitWorktreeEnv(cmd) }

// LinkedWorktree is the exported alias of the unexported linkedWorktree
// classification type, for the white-box test forwarders only.
type LinkedWorktree = linkedWorktree

// LinkedWorktreeMainRoot exposes the mainRoot field read access for the
// staying-root white-box test (TestProbe_IgnoresGitHookEnv), which
// cannot reach the unexported field across the package boundary.
func LinkedWorktreeMainRoot(info *linkedWorktree) string { return info.mainRoot }

// ProjectSkillSubdirsLen forwards len(projectSkillSubdirs) for the
// staying-root discovery test; the slice itself stays unexported.
func ProjectSkillSubdirsLen() int { return len(projectSkillSubdirs) }

// GlobalSkillsDirsForHome forwards globalSkillsDirs(homeDir), the
// homeDir-injected form used by the staying-root discovery test.
func GlobalSkillsDirsForHome(homeDir string) []string { return globalSkillsDirs(homeDir) }
