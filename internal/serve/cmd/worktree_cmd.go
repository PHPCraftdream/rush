// The `rush worktree` command family: removal and listing of the
// rush-managed git worktrees that `rush run --worktree <slug>` creates
// under the main checkout's `worktrees/` directory.
package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var worktreeCmd = &cobra.Command{
	Use:   "worktree",
	Short: "Manage the git worktrees rush run --worktree creates",
	Long: "Manage the git worktrees that `rush run --worktree <slug>` creates " +
		"under the main checkout's `worktrees/` directory.\n\n" +
		"Subcommands:\n" +
		"  remove <slug>  Remove one rush-managed worktree (and by default its branch).\n" +
		"  list           List every rush-managed worktree with its live session.",
}

var worktreeRemoveCmd = &cobra.Command{
	Use:   "remove <slug>",
	Short: "Remove a rush-managed worktree created by rush run --worktree",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runWorktreeRemove,
	Long: "Remove the rush-managed worktree <mainRoot>/worktrees/<slug> that " +
		"`rush run --worktree <slug>` created. <mainRoot> is the main " +
		"checkout of the repository, so removal works even when rush runs " +
		"from inside another linked worktree.\n\n" +

		"Live-run refusal: if a session lock for this worktree is held " +
		"right now (the slug-named session in the main checkout's shared " +
		"<mainRoot>/.rush data directory, or any session lock in the " +
		"worktree's own legacy <wt>/.rush/locks store), removal refuses " +
		"with the holding session's id. This refusal HOLDS EVEN WITH " +
		"--force: stop the run first with `rush sessions kill <id>` or " +
		"let it finish.\n\n" +

		"Dirty tree: a worktree with uncommitted or untracked changes is " +
		"refused; pass --force to remove it anyway (its changes are lost).\n\n" +

		"Branch policy: by default the worktree's branch is deleted with " +
		"`git branch -d`, which only succeeds for merged branches; an " +
		"unmerged branch is kept with a notice and the command still " +
		"succeeds. --force deletes with `git branch -D`, discarding the " +
		"unmerged commits. --keep-branch skips branch deletion entirely. " +
		"A detached worktree has no branch to delete.\n\n" +

		"Windows behavior: removal retries while antivirus scanners or " +
		"indexers keep the directory locked; if a process still holds it " +
		"open the command reports honestly what remains on disk instead " +
		"of claiming success.",
}

var worktreeListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the rush-managed worktrees and their live sessions",
	Args:  cobra.NoArgs,
	RunE:  runWorktreeList,
	Long: "List every git worktree registered under " +
		"<mainRoot>/worktrees, where mainRoot is the main checkout of " +
		"the current repository.\n\n" +
		"Columns: SLUG is the worktree's directory name under worktrees/; " +
		"BRANCH is its checked-out branch, or \"(detached)\"; SESSION is " +
		"the id of a session whose lock is held right now, or \"-\" when " +
		"no live run was detected (a finished session is not listed); " +
		"PATH is the worktree's absolute path.",
}

func init() {
	worktreeRemoveCmd.Flags().Bool("keep-branch", false,
		"Keep the branch after removing the worktree")
	worktreeRemoveCmd.Flags().Bool("force", false,
		"Remove a worktree with uncommitted or untracked changes and "+
			"delete the branch even when unmerged (a live session lock "+
			"still refuses removal)")
	worktreeCmd.AddCommand(worktreeRemoveCmd, worktreeListCmd)
	rootCmd.AddCommand(worktreeCmd)
}

// runWorktreeRemove implements `rush worktree remove <slug>`.
func runWorktreeRemove(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if len(args) != 1 {
		return fmt.Errorf("usage: rush worktree remove <slug> [--keep-branch] [--force]")
	}
	slug := args[0]
	if err := validateWorktreeSlug(slug); err != nil {
		return err
	}
	keepBranch, _ := cmd.Flags().GetBool("keep-branch")
	force, _ := cmd.Flags().GetBool("force")

	prevWd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}
	startDir, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = os.Chdir(prevWd) }()

	mainRoot, err := mainCheckoutRoot(ctx, startDir)
	if err != nil {
		return err
	}
	entries, err := listGitWorktrees(ctx, mainRoot)
	if err != nil {
		return err
	}
	plan, err := planWorktreeRemoval(entries, mainRoot, slug)
	if err != nil {
		return err
	}

	live, err := worktreeLiveSession(ctx, plan.Path, mainRoot)
	if err != nil {
		return fmt.Errorf("could not probe live sessions for worktree %s: %w",
			slug, err)
	}
	if live != "" {
		return fmt.Errorf("worktree %s has a live run: session %s holds a "+
			"session lock; stop it first (`rush sessions kill %s` or let "+
			"the run finish)", slug, live, live)
	}

	if !force {
		dirty, dirtyErr := worktreeIsDirty(ctx, plan.Path)
		if dirtyErr != nil {
			return dirtyErr
		}
		if dirty {
			return fmt.Errorf("worktree %s has uncommitted or untracked "+
				"changes; pass --force to remove it anyway", slug)
		}
	}

	if err := removeGitWorktree(ctx, mainRoot, plan.Path, force,
		func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		}); err != nil {
		return err
	}

	fmt.Fprintf(cmd.OutOrStdout(), "removed worktree %s (%s)\n", slug, plan.Path)

	switch {
	case plan.Branch == "":
		// Detached worktree: there is no branch to delete.
	case keepBranch:
		fmt.Fprintf(cmd.OutOrStdout(), "keeping branch %s\n", plan.Branch)
	case force:
		if err := removeGitBranch(ctx, mainRoot, plan.Branch, true); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "deleted branch %s\n", plan.Branch)
	default:
		if err := removeGitBranch(ctx, mainRoot, plan.Branch, false); err != nil {
			fmt.Fprintf(os.Stderr, "branch %s kept: git refuses to delete "+
				"an unmerged branch (pass --force to delete anyway)\n",
				plan.Branch)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "deleted branch %s\n", plan.Branch)
		}
	}
	return nil
}

// worktreeLiveSession reports the id of a session whose OS-level lock is
// held right now for this worktree: the slug-named session in the MAIN
// checkout's shared data directory (<mainRoot>/.rush — a run inside the
// worktree resolves its data directory there), plus EVERY session lock
// in the worktree's own local <wt>/.rush/locks when that legacy-local
// store exists. Probing is a real OS lock attempt (TryAcquireSessionLock):
// busy means live (the session id is returned); a successful acquisition
// is released immediately (not live); any other error is propagated so
// callers fail closed.
func worktreeLiveSession(ctx context.Context, wtPath, mainRoot string) (string, error) {
	_ = ctx // Probing uses no git; ctx is kept for signature symmetry.

	// The default naming of `rush run --worktree <slug>` is a session
	// named after the slug, in the MAIN checkout's shared data dir.
	probeID := filepath.Base(filepath.Clean(wtPath))
	mainData := filepath.Join(mainRoot, ".rush")
	if err := probeSessionLock(mainData, probeID); err != nil {
		return "", err
	}
	lock, err := session.TryAcquireSessionLock(mainData, probeID)
	if err != nil {
		var busy *session.SessionLockBusyError
		if !errors.As(err, &busy) {
			return "", fmt.Errorf("could not probe session lock for %s: %w",
				probeID, err)
		}
		return probeID, nil
	}
	if err := lock.Release(); err != nil {
		return "", fmt.Errorf("could not release probe lock for %s: %w",
			probeID, err)
	}

	// Legacy-local store: probe EVERY enumerated lock stem there.
	wtData := filepath.Join(wtPath, ".rush")
	stems, err := lockStems(wtData)
	if err != nil {
		return "", err
	}
	for _, stem := range stems {
		if err := probeSessionLock(wtData, stem); err != nil {
			return "", err
		}
		lock, err := session.TryAcquireSessionLock(wtData, stem)
		if err != nil {
			var busy *session.SessionLockBusyError
			if !errors.As(err, &busy) {
				return "", fmt.Errorf("could not probe session lock %s: %w",
					stem, err)
			}
			return stem, nil
		}
		if err := lock.Release(); err != nil {
			return "", fmt.Errorf("could not release probe lock %s: %w",
				stem, err)
		}
	}
	return "", nil
}

// probeSessionLock verifies the locks directory of dataDir is listable
// without creating anything; a missing data dir or locks dir simply
// means "no locks" and is not an error.
func probeSessionLock(dataDir, sessionID string) error {
	locksDir := filepath.Join(dataDir, "locks")
	if _, err := os.Stat(locksDir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("could not inspect session locks in %s: %w",
			locksDir, err)
	}
	return nil
}

// lockStems lists every lock-file stem in dataDir's locks directory. A
// missing data dir or locks dir simply means "no locks"; a data dir copy
// that exists but has no locks is fine too.
func lockStems(dataDir string) ([]string, error) {
	if err := probeSessionLock(dataDir, ""); err != nil {
		return nil, err
	}
	locksDir := filepath.Join(dataDir, "locks")
	matches, err := filepath.Glob(filepath.Join(locksDir, "session-*.lock"))
	if err != nil {
		return nil, fmt.Errorf("could not glob session locks in %s: %w",
			locksDir, err)
	}
	stems := make([]string, 0, len(matches))
	for _, path := range matches {
		base := filepath.Base(path)
		stem := strings.TrimSuffix(strings.TrimPrefix(base, "session-"), ".lock")
		stems = append(stems, stem)
	}
	return stems, nil
}

// worktreeIsDirty reports whether `git status --porcelain` in wtPath
// prints anything.
func worktreeIsDirty(ctx context.Context, wtPath string) (bool, error) {
	out, err := gitIn(ctx, wtPath, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// worktreeSlugOf returns the slug of a rush-managed worktree: slug when
// path sits directly in <mainRoot>/worktrees, ok=false otherwise.
func worktreeSlugOf(mainRoot, path string) (string, bool) {
	parent := filepath.Dir(filepath.Clean(path))
	if !sameWorktreePath(parent, filepath.Join(mainRoot, worktreeDirName)) {
		return "", false
	}
	slug := filepath.Base(filepath.Clean(path))
	if err := validateWorktreeSlug(slug); err != nil {
		return "", false
	}
	return slug, true
}

// runWorktreeList implements `rush worktree list`.
func runWorktreeList(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()

	prevWd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}
	startDir, err := ResolveCwd(cmd)
	if err != nil {
		return err
	}
	defer func() { _ = os.Chdir(prevWd) }()

	mainRoot, err := mainCheckoutRoot(ctx, startDir)
	if err != nil {
		return err
	}
	entries, err := listGitWorktrees(ctx, mainRoot)
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()
	var rows []worktreeEntry
	for _, entry := range entries {
		if slug, ok := worktreeSlugOf(mainRoot, entry.Path); ok && slug != "" {
			rows = append(rows, entry)
		}
	}
	if len(rows) == 0 {
		fmt.Fprintf(out, "no rush-managed worktrees under %s\n",
			filepath.Join(mainRoot, worktreeDirName))
		return nil
	}

	writer := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	for _, entry := range rows {
		session := "-"
		if live, err := worktreeLiveSession(ctx, entry.Path, mainRoot); err != nil {
			session = fmt.Sprintf("? (probe failed: %v)", err)
		} else if live != "" {
			session = live
		}
		branch := entry.Branch
		if entry.Detached {
			branch = "(detached)"
		}
		slug, _ := worktreeSlugOf(mainRoot, entry.Path)
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", slug, branch, session, entry.Path)
	}
	return writer.Flush()
}
