// Git worktree detection used to bound upward searches: the cached
// rev-parse probe, the project-boundary fallback, and symlink-aware
// directory comparison.
package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/platform"
)

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
// too early/late until restart, not silent data corruption. Launch failures
// (git not on PATH, resource errors) are NOT cached; only git's own answers
// are.
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
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			// Git could not be launched; retry on a later call.
			return ""
		}
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

// WorkspaceRoot is the exported form of worktreeRoot (#1142 step C): the app
// and agent layers bind their session service and their turn guard to the
// canonical checkout root of their working directory. An empty result means
// "outside any working tree"; ownership's home/not-home question is a
// SEPARATE flag -- see WorkspaceHome -- and must never be inferred from this
// value being empty or not. One shared implementation keeps the callers from
// drifting into different notions of "the workspace". When git gives no answer
// (it cannot be launched, or it answers non-zero, e.g. "dubious ownership"),
// falls back to walking up for a .git entry so a normal checkout never
// degrades to "". The fallback returns the path as written, not
// symlink-resolved like git's answer; it is reachable only without git's answer.
func WorkspaceRoot(dir string) string {
	if root := worktreeRoot(dir); root != "" {
		return root
	}
	return dotGitRoot(dir)
}

// dotGitRoot walks up from dir looking for a .git entry (file or
// directory), the gitless fallback mirroring internal/log's locateGit.
// It returns "" when no entry exists up to the filesystem root.
func dotGitRoot(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Lstat(filepath.Join(abs, ".git")); err == nil {
			return abs
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// WorkspaceHome reports whether this process owns ITS OWN data directory
// rather than a shared directory handed out to linked worktrees (#1142 step
// C, WS-1: a legacy unbound session row -- workspace_root empty -- is owned
// by home processes only). This is deliberately NOT derived from the
// workspace root: it comes from Load's data-directory resolution (SD-D
// #1143, datadir.go): shared-from-linked -- source shared, or an explicit
// --data-dir / options.data_directory resolving to <main>/.rush inside a
// linked worktree -- is not home; everything else is. Load stores the
// flag exactly once per process and reload never changes it (WS-3). This
// function is the SINGLE source of the flag: app wiring passes its value
// into the session/wake/queue services, and the agent guard and the server
// read it directly, so no call site recomputes it from the workspace root.
func WorkspaceHome() bool {
	return workspaceHomeFlag.Load()
}

// projectBoundary returns the directory at which an upward configuration
// search rooted at dir should stop. It is the git working tree root when
// one can be detected, otherwise dir itself. Returning dir as a
// fallback keeps Rush from silently adopting state files placed above
// the current project.
func projectBoundary(dir string) string {
	dir = canonicalConfigPath(dir)
	if root := worktreeRoot(dir); root != "" {
		return canonicalConfigPath(root)
	}
	return dir
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
