// Workspace ownership (WS-1, task #1142 step C,
// docs/plans/2026-10-01-shared-data-dir.md sec.1): a session belongs to the
// workspace of the process that created it, and only that process may drive
// it. The predicate below is the single Go-side definition of "owns"; the SQL
// ownership filters (GetLastSession, ListPendingRunQueueEntries,
// ListDueWakeSchedules, NextDueWakeScheduleAt) carry the same predicate as
// their (workspace_root, home) parameters -- keep the two in sync.

package session

import (
	"os"
	"path/filepath"
	"strings"
)

// Owns reports whether a process may drive session S. A session is owned when
// its workspace_root equals the process's workspace, or when it is a legacy
// unbound row (”) and the process is a home process -- the owner of its own
// data directory. A checkout inside git has a non-empty workspace root and is
// still a home process: home is NOT derivable from the root (it is false only
// for a linked worktree on the shared data directory, SD-D #1143), and every
// production caller passes config.WorkspaceHome().
//
// procHome is therefore an explicit flag from the app layer, never a
// re-derivation: a home process (every process but a shared-from-linked one)
// must keep driving every pre-existing legacy row, or "--continue", the pump and the wake
// scheduler would all refuse the history they already own.
func Owns(sessWorkspaceRoot, procWorkspaceRoot string, procHome bool) bool {
	if sessWorkspaceRoot == procWorkspaceRoot {
		// Same workspace (a linked worktree may also see rows sharing its
		// root): the normal case, plus the legacy case of two empty roots.
		return true
	}
	if sessWorkspaceRoot != "" {
		// Another workspace's row: never ours, whatever we are.
		return false
	}
	// A legacy '' row: only a home process may run it; a shared-from-linked
	// process (home=false) refuses instead.
	return procHome
}

// gitBranchFromWorkDir reads the branch HEAD points at, resolved against the
// git directory of dir -- WITHOUT spawning git (the design doc forbids a
// subprocess on the session-creation path). A .git DIRECTORY contributes its
// HEAD; a .git FILE ("gitdir: X") the HEAD of X; anything else (no git, an
// unreadable HEAD, a detached HEAD) yields "". Errors are never fatal: the
// branch is audit attribution, not ownership.
func gitBranchFromWorkDir(dir string) string {
	if dir == "" {
		return ""
	}
	gitDir, ok := locateGitDir(dir)
	if !ok {
		return ""
	}
	head, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(head))
	const refPrefix = "ref: "
	if !strings.HasPrefix(line, refPrefix) {
		return "" // detached HEAD
	}
	return strings.TrimPrefix(strings.TrimPrefix(line, refPrefix), "refs/heads/")
}

// locateGitDir walks up from dir looking for the nearest .git entry and
// returns the resolved git directory, following the "gitdir: <path>"
// indirection of linked worktrees and --separate-git-dir.
func locateGitDir(dir string) (string, bool) {
	path := filepath.Clean(dir)
	for {
		entry := filepath.Join(path, ".git")
		fi, err := os.Stat(entry)
		switch {
		case err == nil && fi.IsDir():
			return entry, true
		case err == nil:
			raw, rerr := os.ReadFile(entry)
			if rerr != nil {
				return "", false
			}
			line := strings.TrimSpace(string(raw))
			const prefix = "gitdir: "
			if !strings.HasPrefix(line, prefix) {
				return "", false
			}
			resolved := strings.TrimPrefix(line, prefix)
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(path, resolved)
			}
			return filepath.Clean(resolved), true
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", false
		}
		path = parent
	}
}

// ownsArgs are the (workspace_root, home) parameters every ownership-filtered
// query takes, computed once per call from the service's wiring. The home
// flag is the explicit constructor value (config.WorkspaceHome() in
// production) -- NEVER a re-derivation from the workspace root, which would
// refuse every pre-existing legacy row to every git-checkout process.
func (s *service) ownsArgs() (workspaceRoot string, home int64) {
	return s.workspaceRoot, homeFlag(s.home)
}

func homeFlag(home bool) int64 {
	if home {
		return 1
	}
	return 0
}
