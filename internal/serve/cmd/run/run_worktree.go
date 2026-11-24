// Flag validation and preparation for `rush run --worktree`: resolve or
// create the git worktree, chdir the run into it, default the session id
// to the slug, install the stream mirror, and copy stdout output
// into the main checkout's .rush/stdin at the end.
package run

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	wtree "github.com/PHPCraftdream/rush/internal/serve/cmd/run/wtree"
	"github.com/spf13/cobra"
)

// validateRunWorktreeFlags validates the --worktree/--base/--branch flag
// combination, including conflicts with --cwd. It must be callable when
// --worktree is empty to catch --base/--branch without --worktree.
func validateRunWorktreeFlags(cmd *cobra.Command, slug, base, branch string) error {
	cwdFlag, _ := cmd.Flags().GetString("cwd")
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
	return wtree.ValidateWorktreeSlug(slug)
}

func applyRunWorktreeDataDir(cmd *cobra.Command, mainRoot, sessionID string) error {
	isolated, _ := cmd.Flags().GetBool("isolated-store")
	if !isolated || explicitRunDataDir(cmd) {
		return nil
	}
	flag := cmd.InheritedFlags().Lookup("data-dir")
	if flag == nil {
		flag = cmd.Flags().Lookup("data-dir")
	}
	if flag == nil {
		return fmt.Errorf("--data-dir flag is unavailable for isolated worktree store")
	}
	return flag.Value.Set(filepath.Join(mainRoot, ".rush", "agent-dbs", sessionID, ".rush"))
}

func explicitRunDataDir(cmd *cobra.Command) bool {
	flag := cmd.InheritedFlags().Lookup("data-dir")
	if flag == nil {
		flag = cmd.Flags().Lookup("data-dir")
	}
	return flag != nil && flag.Changed
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
	mirror       *wtree.StreamMirror
	prevCwd      string
	startedAt    time.Time
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
	mainRoot, err := wtree.MainCheckoutRoot(ctx, cwd)
	if err != nil {
		return nil, err
	}
	entries, err := wtree.ListGitWorktrees(ctx, mainRoot)
	if err != nil {
		return nil, err
	}
	wantBranch := branch
	if wantBranch == "" {
		wantBranch = slug
	}
	plan, err := wtree.PlanWorktree(entries, mainRoot, slug, wantBranch,
		func(p string) bool {
			_, err := os.Stat(p)
			return err == nil
		},
		func(b string) bool {
			ok, err := wtree.LocalBranchExists(ctx, mainRoot, b)
			return err == nil && ok
		})
	if err != nil {
		return nil, err
	}

	switch plan.Kind {
	case wtree.WorktreePlanReuse:
		fmt.Fprintf(os.Stderr, "reusing worktree %s (branch %s)\n",
			plan.Path, plan.Branch)
	case wtree.WorktreePlanCreate:
		baseRef := base
		if baseRef == "" {
			baseRef = "HEAD"
		}
		if _, err := wtree.GitIn(ctx, mainRoot, "worktree", "add", "-q",
			"-b", plan.Branch, plan.Path, baseRef); err != nil {
			return nil, err
		}
	case wtree.WorktreePlanCreateExistingBranch:
		if _, err := wtree.GitIn(ctx, mainRoot, "worktree", "add", "-q",
			plan.Path, plan.Branch); err != nil {
			return nil, err
		}
		if base != "" {
			fmt.Fprintf(os.Stderr,
				"note: --base %s is ignored: branch %s already exists\n",
				base, plan.Branch)
		}
	}

	ignored, err := wtree.IsGitIgnored(ctx, mainRoot, plan.Path)
	if err == nil && !ignored {
		fmt.Fprintf(os.Stderr, "warning: %s is not ignored by git — "+
			"add %q to .gitignore to keep worktrees out of git status\n",
			filepath.Join(mainRoot, wtree.WorktreeDirName)+string(os.PathSeparator),
			wtree.WorktreeDirName+"/")
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
	startedAt := time.Now().UTC()
	mirror, err := wtree.StartStreamMirror()
	if err != nil {
		_ = os.Chdir(prevCwd)
		return nil, fmt.Errorf("start stdout/stderr mirroring: %w", err)
	}
	return &runWorktree{
		sessionID:    sessionID,
		mainRoot:     mainRoot,
		worktreePath: plan.Path,
		mirror:       mirror,
		prevCwd:      prevCwd,
		startedAt:    startedAt,
	}, nil
}

// finish stops the mirror, archives prior output when new stdout exists,
// replaces latest stdout and stderr with this run's captured tails, and restores
// the previous working directory.
func (w *runWorktree) finish() {
	outTail, errTail, mirrorErr := w.mirror.Stop()
	if mirrorErr != nil {
		fmt.Fprintf(os.Stderr, "warning: stream mirror: %v\n", mirrorErr)
	}
	dir := filepath.Join(w.mainRoot, ".rush", "stdin")
	if len(outTail) > 0 {
		if err := writeRunWorktreeOutput(dir, w.sessionID, outTail, errTail, w.startedAt); err != nil {
			fmt.Fprintf(os.Stderr, "warning: flushing mirrored run output: %v\n", err)
		}
	} else if err := preserveRunWorktreeStderr(dir, w.sessionID, errTail, w.startedAt); err != nil {
		fmt.Fprintf(os.Stderr, "warning: flushing mirrored run stderr: %v\n", err)
	}
	_ = os.Chdir(w.prevCwd)
}

func preserveRunWorktreeStderr(dir, sessionID string, errTail []byte, startedAt time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	latest := filepath.Join(dir, sessionID+".err")
	old, err := os.ReadFile(latest)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read previous mirrored stderr: %w", err)
	}
	if err == nil {
		archive := filepath.Join(dir, sessionID+"."+startedAt.UTC().Format("20060102T150405.000000000Z")+".err")
		if err := writeRunWorktreeCopy(archive, old); err != nil {
			return fmt.Errorf("archive previous mirrored stderr: %w", err)
		}
	}
	return replaceRunWorktreeFile(latest, errTail)
}

func writeRunWorktreeOutput(dir, sessionID string, outTail, errTail []byte, startedAt time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	stamp := startedAt.UTC().Format("20060102T150405.000000000Z")
	for _, ext := range []string{"out", "err"} {
		latest := filepath.Join(dir, sessionID+"."+ext)
		old, err := os.ReadFile(latest)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read previous mirrored %s: %w", ext, err)
		}
		if err == nil {
			archivePath := filepath.Join(dir, sessionID+"."+stamp+"."+ext)
			if err := writeRunWorktreeCopy(archivePath, old); err != nil {
				return fmt.Errorf("archive previous mirrored %s: %w", ext, err)
			}
		}
	}
	if err := replaceRunWorktreeFile(filepath.Join(dir, sessionID+".out"), outTail); err != nil {
		return fmt.Errorf("write mirrored stdout: %w", err)
	}
	if err := replaceRunWorktreeFile(filepath.Join(dir, sessionID+".err"), errTail); err != nil {
		return fmt.Errorf("write mirrored stderr: %w", err)
	}
	return nil
}

func replaceRunWorktreeFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func writeRunWorktreeCopy(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}
