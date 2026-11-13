// Git plumbing for the `rush run --worktree` feature: listing worktrees,
// resolving the main checkout root, and removing worktrees and branches
// with the Windows-safe retry policy.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
)

// worktreeRemoveAttempts is how many times removeGitWorktree retries a
// removal that git or the filesystem refuses.
const worktreeRemoveAttempts = 5

// worktreeRemoveRetryDelay spaces out the removal retries: Windows keeps
// directories locked by antivirus scanners and search indexers for a
// short while after the owning process exits.
const worktreeRemoveRetryDelay = 300 * time.Millisecond

// worktreeEntry is one record of `git worktree list --porcelain`.
type worktreeEntry struct {
	Path     string // Absolute native-separator cleaned path.
	Head     string // Commit sha, when present.
	Branch   string // Short branch name (no refs/heads/ prefix); "" when detached/bare.
	Bare     bool
	Detached bool
}

// stripHookGitEnv removes the hook-exported git environment variables
// (GIT_DIR, GIT_WORK_TREE, GIT_COMMON_DIR, GIT_INDEX_FILE) from the
// child command's environment. Rush is often launched from inside a git
// hook, and a stale GIT_DIR would redirect every git invocation in this
// file to the wrong repository. This mirrors config.stripGitWorktreeEnv;
// cmd must not import config, so the filter is reimplemented locally.
func stripHookGitEnv(cmd *exec.Cmd) {
	stripped := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true,
		"GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true,
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if i := strings.IndexByte(entry, '='); i > 0 && stripped[entry[:i]] {
			continue
		}
		filtered = append(filtered, entry)
	}
	cmd.Env = filtered
}

// gitIn runs one git command in dir, strips the hook-exported git
// variables from its environment, and returns the trimmed stdout. On
// failure the error names the git arguments and includes stderr.
func gitIn(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := platform.Command(ctx, "git", args...)
	cmd.Dir = dir
	stripHookGitEnv(cmd)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return "", fmt.Errorf("git %s in %s failed: %s: %w",
				strings.Join(args, " "), dir, msg, err)
		}
		return "", fmt.Errorf("git %s in %s failed: %w",
			strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitExitCode extracts the exit code of a finished git command. It
// returns ok=false when the process could not be started or the error is
// not an exit-status error.
func gitExitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if ok := errors.As(err, &exitErr); !ok {
		return 0, false
	}
	return exitErr.ExitCode(), true
}

// listGitWorktrees parses `git worktree list --porcelain` output.
// Records are separated by blank lines; attribute lines are "worktree
// <path>", "HEAD <sha>", "branch refs/heads/<name>", "detached", "bare",
// and "locked ..." (locked lines are ignored). Git prints forward
// slashes on Windows, so each path is converted with filepath.FromSlash,
// made absolute relative to dir, and cleaned.
func listGitWorktrees(ctx context.Context, dir string) ([]worktreeEntry, error) {
	out, err := gitIn(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var entries []worktreeEntry
	var cur *worktreeEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			cur = nil
			continue
		}
		attr, value, _ := strings.Cut(line, " ")
		switch attr {
		case "worktree":
			entries = append(entries, worktreeEntry{})
			cur = &entries[len(entries)-1]
			cur.Path = cleanGitPath(dir, value)
		case "HEAD":
			if cur != nil {
				cur.Head = value
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(value, "refs/heads/")
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("git worktree list returned no records in %s", dir)
	}
	return entries, nil
}

// cleanGitPath converts a slash path reported by git into a cleaned
// absolute native path relative to dir.
func cleanGitPath(dir, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, filepath.FromSlash(p))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Clean(p)
	}
	return filepath.Clean(filepath.FromSlash(p))
}

// mainCheckoutRoot returns the path of the main (first) worktree of the
// repository containing startDir. `git worktree list` always reports the
// main checkout first, so this works when rush itself runs inside a
// linked worktree too. A bare main repository is rejected: rush needs a
// checkout to host the worktrees directory.
func mainCheckoutRoot(ctx context.Context, startDir string) (string, error) {
	entries, err := listGitWorktrees(ctx, startDir)
	if err != nil {
		return "", fmt.Errorf("not a git repository (git worktree list failed in %s): %w",
			startDir, err)
	}
	main := entries[0]
	if main.Bare {
		return "", fmt.Errorf("main repository at %s is bare; rush needs a non-bare checkout",
			main.Path)
	}
	return main.Path, nil
}

// isGitIgnored reports whether absPath is matched by the repository's
// ignore rules, by running `git check-ignore -q -- <absPath>` with the
// command rooted in dir. Exit 0 means ignored, exit 1 means not
// ignored, and anything else is an error.
func isGitIgnored(ctx context.Context, dir, absPath string) (bool, error) {
	cmd := platform.Command(ctx, "git", "check-ignore", "-q", "--", absPath)
	cmd.Dir = dir
	stripHookGitEnv(cmd)
	_, err := cmd.Output()
	if err == nil {
		return true, nil
	}
	if code, ok := gitExitCode(err); ok {
		if code == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git check-ignore exited %d for %s", code, absPath)
	}
	return false, fmt.Errorf("git check-ignore could not start: %w", err)
}

// localBranchExists reports whether refs/heads/<branch> resolves in the
// repository at dir. Exit 0 means the branch exists, exit 1 means it
// does not, and anything else is an error.
func localBranchExists(ctx context.Context, dir, branch string) (bool, error) {
	cmd := platform.Command(ctx, "git", "show-ref", "--verify", "--quiet",
		"refs/heads/"+branch)
	cmd.Dir = dir
	stripHookGitEnv(cmd)
	_, err := cmd.Output()
	if err == nil {
		return true, nil
	}
	if code, ok := gitExitCode(err); ok {
		if code == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git show-ref exited %d for refs/heads/%s", code, branch)
	}
	return false, fmt.Errorf("git show-ref could not start: %w", err)
}

// removeGitBranch deletes a local branch with `git branch -d` (or `-D`
// when force). The error includes git's own message, so callers see the
// "not fully merged" refusal; they distinguish that case via the force
// flag only, so no typed error is needed.
func removeGitBranch(ctx context.Context, dir, branch string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := gitIn(ctx, dir, "branch", flag, branch)
	return err
}

// removeGitWorktree removes a linked worktree at path (registered in the
// repository rooted at mainRoot). It retries `git worktree remove` up to
// worktreeRemoveAttempts times because Windows keeps directories locked
// by antivirus scanners and indexers. The os.RemoveAll fallback (with
// the same retry policy) runs ONLY when force is true: with force=false
// git refuses dirty worktrees and that refusal is honored, returning an
// error that hints at --force. After a successful removal the
// registration is pruned, and the function finally refuses to claim
// success if the directory is still on disk.
func removeGitWorktree(
	ctx context.Context, mainRoot, path string, force bool,
	logf func(format string, args ...any),
) error {
	removeArgs := []string{"worktree", "remove"}
	if force {
		removeArgs = append(removeArgs, "--force")
	}
	removeArgs = append(removeArgs, path)

	var gitErr error
	for attempt := 1; attempt <= worktreeRemoveAttempts; attempt++ {
		_, err := gitIn(ctx, mainRoot, removeArgs...)
		if err == nil {
			gitErr = nil
			break
		}
		gitErr = err
		if attempt < worktreeRemoveAttempts {
			logf("retrying worktree removal (%d/%d): %v",
				attempt, worktreeRemoveAttempts, err)
			time.Sleep(worktreeRemoveRetryDelay)
		}
	}

	fallback := false
	if gitErr != nil && !force {
		// Without force, git refuses dirty worktrees; do not prune (the
		// registration is still valid) and never touch the filesystem.
		return fmt.Errorf("git refused to remove worktree %s (use --force to remove it anyway): %w",
			path, gitErr)
	}
	if gitErr != nil {
		fallback = true
		for attempt := 1; attempt <= worktreeRemoveAttempts; attempt++ {
			err := os.RemoveAll(path)
			if err == nil {
				break
			}
			if attempt < worktreeRemoveAttempts {
				logf("retrying worktree removal (%d/%d): %v",
					attempt, worktreeRemoveAttempts, err)
				time.Sleep(worktreeRemoveRetryDelay)
			}
		}
	}

	pruneErr := gitPrune(ctx, mainRoot)
	if fallback {
		// The fallback already had to force things; a prune failure is
		// cosmetic next to that, so it is ignored here.
		_ = pruneErr
	} else if pruneErr != nil {
		return fmt.Errorf("worktree removed but git worktree prune failed: %w", pruneErr)
	}

	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf(
			"worktree directory %s still exists after removal; "+
				"a process is holding it open — close the locking process and delete it manually",
			path)
	}
	if gitErr != nil {
		return fmt.Errorf("worktree %s removed with fallback after git refused: %w",
			path, gitErr)
	}
	return nil
}

// gitPrune drops stale worktree registrations with `git worktree prune`.
func gitPrune(ctx context.Context, mainRoot string) error {
	_, err := gitIn(ctx, mainRoot, "worktree", "prune")
	return err
}
