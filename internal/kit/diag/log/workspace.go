// Workspace attribution for log lines: the canonical checkout root of
// the process and the branch it currently has checked out. Both are
// derived without spawning git, by locating the nearest .git entry and
// reading HEAD directly.
package log

import (
	"os"
	"path/filepath"
	"strings"
)

// WorkspaceRoot returns the canonical root of the checkout the process
// runs in: the git working tree root when one can be located, otherwise
// the canonical current working directory. It is the same notion as
// sessions.workspace_root (see docs/plans/2026-10-01-shared-data-dir.md)
// and is meant to be stamped as the constant `ws` attribute on every
// log line, so records from linked worktrees sharing one log file can
// be told apart.
func WorkspaceRoot() string {
	wd := cwdOr("")
	toplevel, _ := locateGit(wd)
	if toplevel != "" {
		return canonicalDir(toplevel)
	}
	if wd == "" {
		return ""
	}
	return canonicalDir(wd)
}

// currentBranch returns the branch name HEAD points at, resolved
// against the nearest git directory of the current working directory.
// A detached HEAD, a missing or unreadable HEAD, or not being in a git
// checkout at all all yield "" — the attribute is best-effort
// attribution, never a reason to fail logging.
func currentBranch() string {
	_, gitDir := locateGit(cwdOr(""))
	if gitDir == "" {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(head))
	const prefix = "ref: "
	if !strings.HasPrefix(line, prefix) {
		return "" // detached HEAD
	}
	ref := strings.TrimPrefix(line, prefix)
	return strings.TrimPrefix(ref, "refs/heads/")
}

// locateGit walks up from dir looking for a .git entry. It returns the
// working tree root (the directory containing .git) and the resolved
// git directory (following the `gitdir: <path>` indirection used by
// linked worktrees and --separate-git-dir). Both are "" when dir is
// not inside a checkout or the entry cannot be resolved.
func locateGit(dir string) (toplevel, gitDir string) {
	if dir == "" {
		return "", ""
	}
	path := dir
	for {
		entry := filepath.Join(path, ".git")
		fi, err := os.Stat(entry)
		switch {
		case err == nil && fi.IsDir():
			return path, entry
		case err == nil:
			// .git file: "gitdir: <path>", relative to the file.
			raw, rerr := os.ReadFile(entry)
			if rerr != nil {
				return "", ""
			}
			line := strings.TrimSpace(string(raw))
			const prefix = "gitdir: "
			if !strings.HasPrefix(line, prefix) {
				return "", ""
			}
			resolved := strings.TrimPrefix(line, prefix)
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(path, resolved)
			}
			return path, filepath.Clean(resolved)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", ""
		}
		path = parent
	}
}

// canonicalDir resolves symlinks best-effort so the same physical
// checkout always yields the same ws value regardless of the path the
// process was started from.
func canonicalDir(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return filepath.Clean(dir)
}

// cwdOr returns the process working directory, or fallback when it
// cannot be determined (e.g. the directory was removed).
func cwdOr(fallback string) string {
	wd, err := os.Getwd()
	if err != nil {
		return fallback
	}
	return wd
}
