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
// go.mod declares module github.com/charmbracelet/crush.
func IsInSourceTree(exePath string) bool {
	// Check for marker directories in the path.
	// We walk up the path looking for "dev" or ".claude/worktrees".
	for dir := filepath.Dir(exePath); dir != exePath; dir = filepath.Dir(dir) {
		if dir == "" || dir == "." || dir == filepath.Dir(dir) {
			break
		}

		base := filepath.Base(dir)
		if base == "dev" || base == "worktrees" {
			// Found a potential marker. For "worktrees", verify it has a ".claude" parent.
			if base == "worktrees" {
				parent := filepath.Base(filepath.Dir(dir))
				if parent != ".claude" {
					continue // Not a .claude/worktrees marker, keep looking.
				}
			}

			// The ancestor candidate is the marker directory itself (for dev) or
			// the parent of .claude (for worktrees). We start checking here and walk up.
			var ancestor string
			if base == "dev" {
				ancestor = dir // Check the dev directory itself first.
			} else {
				// For .claude/worktrees, start at the parent of .claude (skip the marker dir).
				ancestor = filepath.Dir(filepath.Dir(dir))
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
				if module == "github.com/charmbracelet/crush" {
					return true // Detected!
				}
				// Different module — stop, don't walk past it.
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
		return fmt.Errorf("crush binary is running from inside its own source tree: %s\n\n"+
			"This looks like a scratch dev build inside the crush repo that can "+
			"silently go stale relative to the source it exercises.\n\n"+
			"To run crush, use your installed/system crush instead, or rebuild and "+
			"reinstall fresh via:\n"+
			"  go install github.com/charmbracelet/crush@latest\n\n"+
			"Or build from source with 'go build .' from a clone and move the binary "+
			"OUT of the repo before use.\n",
			absPath)
	}

	return nil
}

// readModuleLine reads the first line from go.mod that starts with "module ".
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
		if strings.HasPrefix(line, "module ") {
			return line, nil
		}
	}
	return "", os.ErrNotExist // No module line found, treat as not existing.
}

// parseModuleFromLine extracts the module name from a "module" line.
// Handles both quoted and bare module names.
func parseModuleFromLine(line string) string {
	// Trim leading/trailing whitespace first.
	line = strings.TrimSpace(line)

	// Remove the "module " prefix.
	modulePart := strings.TrimSpace(strings.TrimPrefix(line, "module "))

	// Strip optional surrounding quotes.
	modulePart = strings.Trim(modulePart, `"`)

	return modulePart
}
