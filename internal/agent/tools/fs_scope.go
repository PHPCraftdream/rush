package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/filepathext"
)

// resolveScopedPath resolves a raw (absolute or workingDir-relative) path
// into an absolute, symlink-resolved form suitable for
// permission.FolderScope.Check (or for pre-resolving a
// FolderScopeEntry.Dir before compilation).
//
// Only the longest EXISTING prefix of the path is symlink-resolved; the
// non-existent tail is appended unchanged. This produces a deliberate
// asymmetry: a create on a not-yet-existing file is judged by its
// RESOLVED PARENT directory (catching a symlinked parent that escapes a
// granted scope), while a read/write/delete on an existing file is
// judged by its own fully-resolved path (catching a symlink pointing
// outside the scope directly). Any ambiguity — an unreadable component,
// a file used as a directory, or a failed symlink evaluation — is an
// error, never a silently-produced path, so consumers fail closed.
func resolveScopedPath(ctx context.Context, disk DiskProvider, workingDir, raw string) (abs string, err error) {
	disk = diskOrOS(disk)
	cleaned, err := filepath.Abs(filepathext.SmartJoin(workingDir, raw))
	if err != nil {
		return "", fmt.Errorf("fs: resolve %q: %w", raw, err)
	}
	cleaned = filepath.Clean(cleaned)

	// Walk up to the longest existing prefix. Anything other than a
	// clean miss (EACCES, ELOOP, ...) is unresolvable: fail closed.
	prefix := cleaned
	for {
		info, statErr := disk.Stat(ctx, prefix)
		if statErr == nil {
			if !info.IsDir() && prefix != cleaned {
				return "", fmt.Errorf(
					"fs: resolve %q: path component is a file, not a directory: %s",
					raw, prefix)
			}
			break
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", fmt.Errorf("fs: resolve %q: stat %s: %w", raw, prefix, statErr)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			break
		}
		prefix = parent
	}

	resolvedPrefix, err := disk.EvalSymlinks(ctx, prefix)
	if err != nil {
		// A prefix we cannot evaluate cannot be judged safely.
		return "", fmt.Errorf("fs: resolve %q: evaluate symlinks in %s: %w",
			raw, prefix, err)
	}

	tail, err := filepath.Rel(prefix, cleaned)
	if err != nil {
		return "", fmt.Errorf("fs: resolve %q: %w", raw, err)
	}
	return filepath.Join(resolvedPrefix, tail), nil
}
