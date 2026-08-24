// Git worktree detection used to bound upward searches: the cached
// rev-parse probe, the project-boundary fallback, and symlink-aware
// directory comparison.
package config

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/platform"
)

func isInsideWorktree() bool {
	cmd := platform.Command(
		context.Background(),
		"git", "rev-parse",
		"--is-inside-work-tree",
	)
	bts, err := cmd.CombinedOutput()
	return err == nil && strings.TrimSpace(string(bts)) == "true"
}

// worktreeRootCache memoizes worktreeRoot's per-directory result. Every
// call spawns a real `git rev-parse --show-toplevel` child process; profiled
// (task #452, following up on task #450's test-speed investigation) at
// ~11 spawns per internal/cmd test through this function's callers
// (lookupConfigs -> projectBoundary, setDefaults -> projectBoundary,
// ProjectSkillsDir), each called multiple times per Load and Load itself
// running several times per test.
//
// Keyed on dir (as passed in, not canonicalized) and cached for the life of
// the process: a directory's git-worktree membership does not change while
// rush is running under any normal workflow (unlike cliprovider.Available's
// PATH-keyed cache, which DOES need to invalidate on a PATH change a running
// process can legitimately observe, worktreeRoot has no equivalent
// externally-observable "this changed" signal to key on). The one scenario
// this trades away — running `git worktree add/remove` or `git init` on a
// directory rush already resolved a boundary for, in the same long-lived
// process, without restarting — is the same class of staleness
// cliprovider.detectAvailable's cache already accepts for a newly-installed
// CLI (see that function's doc comment). Every caller (lookupConfigs,
// setDefaults, ProjectSkillsDir) treats worktreeRoot purely as a config-file
// SEARCH BOUNDARY, never as a live git status source of truth — a stale
// boundary would at worst mean an upward config search stops one directory
// too early/late until restart, not silent data corruption.
var worktreeRootCache sync.Map // map[string]string

// worktreeRoot returns the absolute path of the git working tree root for
// dir, or the empty string if dir is not inside a working tree (bare
// repositories, missing git binary, plain directories, or any other
// failure mode). Linked worktrees and submodules each report their own
// top-level, which is what callers want when bounding lookups.
func worktreeRoot(dir string) string {
	if cached, ok := worktreeRootCache.Load(dir); ok {
		return cached.(string)
	}

	cmd := platform.Command(
		context.Background(),
		"git", "rev-parse", "--show-toplevel",
	)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		worktreeRootCache.Store(dir, "")
		return ""
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		worktreeRootCache.Store(dir, "")
		return ""
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		worktreeRootCache.Store(dir, "")
		return ""
	}
	worktreeRootCache.Store(dir, abs)
	return abs
}

// projectBoundary returns the directory at which an upward configuration
// search rooted at dir should stop. It is the git working tree root when
// one can be detected, otherwise dir itself. Returning dir as a
// fallback keeps Rush from silently adopting state files placed above
// the current project.
func projectBoundary(dir string) string {
	if root := worktreeRoot(dir); root != "" {
		return root
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}

// sameDir reports whether a and b refer to the same directory, accounting
// for symbolic links. Paths are canonicalised with filepath.EvalSymlinks
// before comparison so that a symlinked path (macOS /var) and its resolved
// target (/private/var) compare equal. If either path cannot be resolved
// (e.g. it does not exist), the raw value is used as a best-effort fallback.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return a == b
}
