// Flag validation and preparation for `rush run --worktree`: resolve or
// create the git worktree, chdir the run into it, default the session id
// to the slug, install the stream mirror, and flush the mirrored
// stdout/stderr into the main checkout's .rush/stdin at the end.
package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// validateRunWorktreeFlags validates the --worktree/--base/--branch flag
// combination, including the conflicts with the root --cwd/--data-dir
// flags. It must be callable even when --worktree is empty (to catch
// --base/--branch without --worktree). Reads --cwd and --data-dir from
// the command's flags.
func validateRunWorktreeFlags(cmd *cobra.Command, slug, base, branch string) error {
	cwdFlag, _ := cmd.Flags().GetString("cwd")
	dataDirFlag, _ := cmd.Flags().GetString("data-dir")
	switch {
	case slug == "" && base == "" && branch == "":
		return nil
	case slug == "":
		return fmt.Errorf("--base/--branch require --worktree")
	}
	if cwdFlag != "" {
		return fmt.Errorf("--worktree is incompatible with --cwd: " +
			"the worktree defines the working directory")
	}
	if dataDirFlag != "" {
		return fmt.Errorf("--worktree is incompatible with --data-dir: " +
			"the run uses the data directory resolved from inside the " +
			"worktree (the shared <main>/.rush)")
	}
	return validateWorktreeSlug(slug)
}

// validateMirrorSessionID rejects session ids that could escape the
// .rush/stdin directory: empty, containing / or \, or "." / "..".
func validateMirrorSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("mirror session id must not be empty")
	}
	if strings.ContainsAny(sessionID, `/\`) {
		return fmt.Errorf("mirror session id %q must not contain / or \\",
			sessionID)
	}
	if sessionID == "." || sessionID == ".." {
		return fmt.Errorf("mirror session id %q must not be %q", sessionID,
			sessionID)
	}
	return nil
}

// runWorktree is the per-run state of the --worktree feature: where the
// run executes, which mirror is installed, and what to restore in
// finish().
type runWorktree struct {
	sessionID    string
	mainRoot     string
	worktreePath string
	mirror       *streamMirror
	prevCwd      string
}

// prepareRunWorktree resolves or creates the worktree, chdirs into it,
// defaults the session id, and installs the stream mirror. Called only
// when slug != "".
func prepareRunWorktree(
	ctx context.Context, cmd *cobra.Command, slug, base, branch, sessionID string,
) (*runWorktree, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("resolve current directory: %w", err)
	}
	mainRoot, err := mainCheckoutRoot(ctx, cwd)
	if err != nil {
		return nil, err
	}
	entries, err := listGitWorktrees(ctx, mainRoot)
	if err != nil {
		return nil, err
	}
	wantBranch := branch
	if wantBranch == "" {
		wantBranch = slug
	}
	plan, err := planWorktree(entries, mainRoot, slug, wantBranch,
		func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		func(b string) bool {
			ok, err := localBranchExists(ctx, mainRoot, b)
			return err == nil && ok
		})
	if err != nil {
		return nil, err
	}

	switch plan.Kind {
	case worktreePlanReuse:
		fmt.Fprintf(os.Stderr, "reusing worktree %s (branch %s)\n",
			plan.Path, plan.Branch)
	case worktreePlanCreate:
		baseRef := base
		if baseRef == "" {
			baseRef = "HEAD"
		}
		if _, err := gitIn(ctx, mainRoot, "worktree", "add", "-q",
			"-b", plan.Branch, plan.Path, baseRef); err != nil {
			return nil, err
		}
	case worktreePlanCreateExistingBranch:
		if _, err := gitIn(ctx, mainRoot, "worktree", "add", "-q",
			plan.Path, plan.Branch); err != nil {
			return nil, err
		}
		if base != "" {
			fmt.Fprintf(os.Stderr,
				"note: --base %s is ignored: branch %s already exists\n",
				base, plan.Branch)
		}
	}

	ignored, err := isGitIgnored(ctx, mainRoot, plan.Path)
	if err == nil && !ignored {
		fmt.Fprintf(os.Stderr, "warning: %s is not ignored by git — "+
			"add %q to .gitignore to keep worktrees out of git status\n",
			filepath.Join(mainRoot, worktreeDirName)+string(os.PathSeparator),
			worktreeDirName+"/")
	}

	prevCwd := cwd
	if err := os.Chdir(plan.Path); err != nil {
		return nil, fmt.Errorf("chdir into worktree %s: %w", plan.Path, err)
	}
	if sessionID == "" {
		sessionID = slug
	}
	if err := validateMirrorSessionID(sessionID); err != nil {
		_ = os.Chdir(prevCwd)
		return nil, err
	}
	mirror, err := startStreamMirror()
	if err != nil {
		// A mirror failure must not leave the cwd changed.
		_ = os.Chdir(prevCwd)
		return nil, fmt.Errorf("start stdout/stderr mirroring: %w", err)
	}
	return &runWorktree{
		sessionID:    sessionID,
		mainRoot:     mainRoot,
		worktreePath: plan.Path,
		mirror:       mirror,
		prevCwd:      prevCwd,
	}, nil
}

// finish stops the mirror, flushes the tails into the main checkout's
// .rush/stdin/<session>.{out,err}, and restores the previous working
// directory. It never panics and never masks the run's own result: flush
// errors go to stderr as warnings.
func (w *runWorktree) finish() {
	outTail, errTail, merr := w.mirror.stop()
	if merr != nil {
		fmt.Fprintf(os.Stderr, "warning: stopping stream mirror: %v\n", merr)
	}
	if err := flushRunWorktreeMirror(w.mainRoot, w.sessionID, outTail, errTail); err != nil {
		fmt.Fprintf(os.Stderr, "warning: flushing mirrored run output: %v\n", err)
	}
	_ = os.Chdir(w.prevCwd)
}

// flushRunWorktreeMirror writes the mirrored streams into
// <mainRoot>/.rush/stdin/<sessionID>.out and .err (MkdirAll 0o755,
// WriteFile 0o644, files always written even when empty).
func flushRunWorktreeMirror(mainRoot, sessionID string, outTail, errTail []byte) error {
	dir := filepath.Join(mainRoot, ".rush", "stdin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".out"), outTail, 0o644); err != nil {
		return fmt.Errorf("write mirrored stdout: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, sessionID+".err"), errTail, 0o644); err != nil {
		return fmt.Errorf("write mirrored stderr: %w", err)
	}
	return nil
}
