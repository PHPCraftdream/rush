// Pure decisions for the `rush run --worktree` feature: slug validation,
// target path containment, and the create/reuse/remove planning that
// callers drive with injected existence checks.
package cmd

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// worktreeDirName is the subdirectory of the main checkout holding all
// rush-managed linked worktrees.
const worktreeDirName = "worktrees"

// worktreeSlugRE restricts slugs to single path segments safe on every
// platform: lowercase letters, digits, dots, underscores and hyphens,
// none of them leading.
var worktreeSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// windowsReservedNames are device names Windows reserves case-
// insensitively, with or without an extension; a slug matching one of
// them would create an unusable (or dangerous) directory.
var windowsReservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// validateWorktreeSlug rejects slugs that cannot name a directory under
// <main>/worktrees: empty values, anything outside the slug regex
// (uppercase, leading punctuation, separators, spaces), any value
// containing "..", and Windows reserved device names with or without an
// extension.
func validateWorktreeSlug(slug string) error {
	if slug == "" {
		return fmt.Errorf("worktree slug must not be empty")
	}
	if strings.Contains(slug, "..") {
		return fmt.Errorf("worktree slug %q must not contain %q", slug, "..")
	}
	if !worktreeSlugRE.MatchString(slug) {
		return fmt.Errorf("worktree slug %q must match %s (lowercase letters, "+
			"digits, dots, underscores, hyphens; no leading punctuation, "+
			"separators, spaces or uppercase)", slug, worktreeSlugRE.String())
	}
	base, _, _ := strings.Cut(slug, ".")
	if windowsReservedNames[strings.ToLower(base)] {
		return fmt.Errorf("worktree slug %q uses a Windows reserved device name",
			slug)
	}
	return nil
}

// worktreeTargetPath resolves <mainRoot>/worktrees/<slug> for a valid
// slug. The containment check with filepath.Rel is defense in depth: if
// the relative path escapes the worktrees directory the slug filter was
// somehow bypassed and the request is refused.
func worktreeTargetPath(mainRoot, slug string) (string, error) {
	if err := validateWorktreeSlug(slug); err != nil {
		return "", err
	}
	target := filepath.Join(mainRoot, worktreeDirName, slug)
	rel, err := filepath.Rel(filepath.Join(mainRoot, worktreeDirName), target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("worktree target %q escapes %q",
			target, filepath.Join(mainRoot, worktreeDirName))
	}
	return target, nil
}

// sameWorktreePath compares two paths after cleaning, folding case only
// on Windows where the filesystem comparison is case-insensitive.
func sameWorktreePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if isWindowsHost() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// isWindowsHost reports whether rush is running on Windows.
func isWindowsHost() bool {
	return runtime.GOOS == "windows"
}

// worktreePlanKind selects the git command sequence a plan needs.
type worktreePlanKind int

const (
	// worktreePlanCreate: git worktree add -b <branch> <path> <base>.
	worktreePlanCreate worktreePlanKind = iota
	// worktreePlanCreateExistingBranch: the branch already exists, so
	// `git worktree add` runs without -b.
	worktreePlanCreateExistingBranch
	// worktreePlanReuse: a registered worktree already sits at the
	// target path and its branch matches.
	worktreePlanReuse
)

// worktreePlan is the decision produced by planWorktree.
type worktreePlan struct {
	Kind   worktreePlanKind
	Path   string
	Branch string
	Entry  worktreeEntry // Set for Reuse.
}

// planWorktree decides what `rush run --worktree <slug>` must do.
// pathExists and branchExists are injected so this function stays pure.
func planWorktree(
	entries []worktreeEntry, mainRoot, slug, wantBranch string,
	pathExists, branchExists func(string) bool,
) (worktreePlan, error) {
	target, err := worktreeTargetPath(mainRoot, slug)
	if err != nil {
		return worktreePlan{}, err
	}
	for _, entry := range entries {
		if !sameWorktreePath(entry.Path, target) {
			continue
		}
		if entry.Branch == wantBranch {
			return worktreePlan{
				Kind: worktreePlanReuse, Path: target,
				Branch: wantBranch, Entry: entry,
			}, nil
		}
		if entry.Detached {
			return worktreePlan{}, fmt.Errorf(
				"worktree %s is detached; pass --branch <name> naming its branch",
				target)
		}
		return worktreePlan{}, fmt.Errorf(
			"worktree %s is on branch %s, not %s; pass --branch %s to reuse it",
			target, entry.Branch, wantBranch, entry.Branch)
	}
	if pathExists(target) {
		return worktreePlan{}, fmt.Errorf(
			"path %s exists but is not a registered git worktree; refusing to overwrite",
			target)
	}
	if branchExists(wantBranch) {
		return worktreePlan{
			Kind: worktreePlanCreateExistingBranch, Path: target,
			Branch: wantBranch,
		}, nil
	}
	return worktreePlan{
		Kind: worktreePlanCreate, Path: target,
		Branch: wantBranch,
	}, nil
}

// removalPlan is the decision produced by planWorktreeRemoval.
type removalPlan struct {
	Entry  worktreeEntry
	Path   string
	Branch string // "" when detached.
}

// planWorktreeRemoval resolves the registered worktree to remove for a
// slug, guarding that the target really sits under <mainRoot>/worktrees.
func planWorktreeRemoval(entries []worktreeEntry, mainRoot, slug string) (removalPlan, error) {
	target, err := worktreeTargetPath(mainRoot, slug)
	if err != nil {
		return removalPlan{}, err
	}
	for _, entry := range entries {
		if !sameWorktreePath(entry.Path, target) {
			continue
		}
		// Belt and braces over the slug check: the registered path must
		// also be contained in the worktrees directory.
		rel, relErr := filepath.Rel(filepath.Join(mainRoot, worktreeDirName), entry.Path)
		if relErr != nil || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return removalPlan{}, fmt.Errorf(
				"registered worktree %q escapes %q",
				entry.Path, filepath.Join(mainRoot, worktreeDirName))
		}
		branch := entry.Branch
		if entry.Detached {
			branch = ""
		}
		return removalPlan{Entry: entry, Path: entry.Path, Branch: branch}, nil
	}
	return removalPlan{}, fmt.Errorf("%s is not a rush-managed worktree under %s",
		slug, filepath.Join(mainRoot, worktreeDirName))
}
