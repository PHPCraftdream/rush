package platform

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IsInSourceTree reports whether exePath (already an absolute, symlink-resolved
// executable path) sits inside this repository's own source tree, i.e. under a
// `dev/` or `.claude/worktrees/` path component within a directory tree whose
// go.mod declares module github.com/PHPCraftdream/rush. Repo-root build outputs
// (`go build .` writing to the clone root) are intentionally NOT covered: deploy
// tooling builds there and copies the binary out, so guarding the root would break it.
func IsInSourceTree(exePath string) bool {
	// Check for marker directories in the path.
	// We walk up the path looking for "dev" or ".claude/worktrees".
	for dir := filepath.Dir(exePath); dir != exePath; dir = filepath.Dir(dir) {
		if dir == "" || dir == "." || dir == filepath.Dir(dir) {
			break
		}

		base := filepath.Base(dir)
		// P3-5(c): Case-insensitive marker directory comparison
		if strings.EqualFold(base, "dev") || strings.EqualFold(base, "worktrees") {
			// Found a potential marker. For "worktrees", verify it has a ".claude" parent.
			if strings.EqualFold(base, "worktrees") {
				parent := filepath.Base(filepath.Dir(dir))
				if !strings.EqualFold(parent, ".claude") {
					continue // Not a .claude/worktrees marker, keep looking.
				}
			}

			// P3-5(a) & (b): Start the go.mod walk AT the marker directory itself for both markers.
			// For worktrees, we also need to check if the exe is inside a specific worktree subdirectory
			// (which has its own go.mod) and use that as the ancestor instead.
			var ancestor string
			if strings.EqualFold(base, "worktrees") {
				// Check if exe is inside a subdirectory of worktrees/
				// If exePath starts with dir+separator, then exe is inside worktrees/
				if strings.HasPrefix(exePath, dir+string(filepath.Separator)) {
					// Extract the relative path from worktrees/ to exe
					relPath := strings.TrimPrefix(exePath, dir+string(filepath.Separator))
					// Get the first component (the worktree name)
					parts := strings.SplitN(relPath, string(filepath.Separator), 2)
					if len(parts) > 0 && parts[0] != "" {
						worktreeName := parts[0]
						worktreeDir := filepath.Join(dir, worktreeName)
						// Check if this worktree dir has a go.mod
						if _, err := os.Stat(filepath.Join(worktreeDir, "go.mod")); err == nil {
							// Use the worktree dir as ancestor
							ancestor = worktreeDir
						} else {
							ancestor = dir
						}
					} else {
						ancestor = dir
					}
				} else {
					ancestor = dir // exe is at worktrees/ level (unlikely)
				}
			} else {
				ancestor = dir // For dev/, use dev/ itself
			}

			// Walk up from the ancestor, checking each directory for go.mod.
			for checkDir := ancestor; checkDir != ""; checkDir = filepath.Dir(checkDir) {
				if checkDir == "." || checkDir == filepath.Dir(checkDir) {
					// Reached the root.
					break
				}

				goModPath := filepath.Join(checkDir, "go.mod")
				moduleLine, err := readModuleLine(goModPath)
				if err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue // No go.mod here, keep walking up.
					}
					// Read error (e.g., permission), stop walking this branch.
					break
				}

				// Found a go.mod — check if it's the crush module.
				module := parseModuleFromLine(moduleLine)

				// P3-5(a): If this go.mod is at the marker dir itself (ancestor), and it's
				// not the crush module, continue walking up. Otherwise, stop.
				if checkDir == ancestor {
					if module == "github.com/PHPCraftdream/rush" {
						return true // Rush module at marker dir itself!
					}
					// Foreign or empty go.mod at marker dir — continue up.
					continue
				}

				// For any level above the marker dir, stop if module is not crush.
				if module == "github.com/PHPCraftdream/rush" {
					return true // Detected!
				}
				// Different module (above marker dir) — stop, don't walk past it.
				break
			}
		}
	}

	return false
}

// GuardSourceTreeRun checks the running executable via os.Executable() +
// filepath.EvalSymlinks and, if inside the source tree, prints an actionable
// error to stderr and returns an error. Cheap no-op otherwise (return nil on
// any resolution failure).
func GuardSourceTreeRun() error {
	exePath, err := os.Executable()
	if err != nil {
		return nil // Resolution failure, no-op.
	}

	// Resolve symlinks to get the true path.
	resolvedPath, err := filepath.EvalSymlinks(exePath)
	if err != nil {
		return nil // Resolution failure, no-op.
	}

	// Convert to absolute path if not already.
	absPath, err := filepath.Abs(resolvedPath)
	if err != nil {
		return nil // Resolution failure, no-op.
	}

	if IsInSourceTree(absPath) {
		return fmt.Errorf("%s", sourceTreeGuardMessage(absPath))
	}

	return nil
}

// sourceTreeGuardMessage returns the error message displayed when running
// from inside the source tree.
func sourceTreeGuardMessage(absPath string) string {
	return fmt.Sprintf("crush binary is running from inside its own source tree: %s\n\n"+
		"This looks like a scratch dev build inside the crush repo that can "+
		"silently go stale relative to the source it exercises.\n\n"+
		"To run crush, use your installed/system crush instead, or rebuild and "+
		"reinstall fresh via:\n"+
		"  npm install -g @phpcraftdream/crush\n\n"+
		"Or rebuild from source with 'go run deploy.go' to build and replace "+
		"your PATH binary, or use 'go build .' from a clone and move the binary "+
		"OUT of the repo before use.",
		absPath)
}

// readModuleLine reads the first line from go.mod that starts with "module " or "module\t".
// Returns ("", nil) if the file exists but has no recognizable module line; the caller
// (IsInSourceTree) treats that like any other non-crush module: a boundary that stops
// the walk when the go.mod sits ABOVE the marker directory, but "foreign, keep
// looking" when it sits AT the marker itself (same treatment as an explicitly
// foreign module at the marker; see the P3-5(a) comment in IsInSourceTree).
// Returns an error if the file doesn't exist or can't be read.
func readModuleLine(goModPath string) (string, error) {
	f, err := os.Open(goModPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		// P3-5(d1): Accept both space and tab separators after "module"
		if strings.HasPrefix(line, "module ") || strings.HasPrefix(line, "module\t") {
			return line, nil
		}
	}
	// P3-5(d2): No module line found — file exists but is unparseable. The
	// caller (IsInSourceTree) treats this the same as any other non-crush
	// module: a boundary above the marker, "foreign, keep looking" at it.
	return "", nil
}

// parseModuleFromLine extracts the module name from a "module" line.
// Handles both quoted and bare module names, and both space and tab separators.
func parseModuleFromLine(line string) string {
	// Trim leading/trailing whitespace first.
	line = strings.TrimSpace(line)

	// P3-5(d1): Remove the "module" prefix (handles both space and tab).
	// Safe because readModuleLine already validated the prefix.
	modulePart := strings.TrimPrefix(line, "module")
	modulePart = strings.TrimSpace(modulePart)

	// Strip optional surrounding quotes.
	modulePart = strings.Trim(modulePart, `"`)

	return modulePart
}
