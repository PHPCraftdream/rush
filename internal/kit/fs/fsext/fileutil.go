package fsext

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/charlievieth/fastwalk"
	"github.com/charmbracelet/x/ansi"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// AtomicWriteFile writes data to path atomically: write to a unique temp file
// in the same directory, fsync, then rename over the destination. A crash
// (kill -9, OOM, power loss) cannot leave the destination half-written —
// either the old content is still there, or the new content is fully there.
//
// Required for parallel-process workflows where the orchestrator may SIGKILL
// children that exceed budget: without this, a write/edit tool call killed
// mid-flight truncates the user's file and silently loses content (the DB
// history snapshot is also taken AFTER the write, so there's no recovery).
//
// On Windows the rename can occasionally fail if another process has the
// destination open with a sharing-violation lock; the temp file is cleaned
// up and the original error returned so the caller surfaces it as a normal
// tool error. We don't retry because retries can mask real bugs.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("atomic write: create temp: %w", err)
	}
	tmp := f.Name()
	cleanup := func() { _ = os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("atomic write: write temp: %w", err)
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("atomic write: chmod temp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return fmt.Errorf("atomic write: fsync temp: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("atomic write: close temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		cleanup()
		return fmt.Errorf("atomic write: rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}

// Expand is a wrapper around [expand.Literal]. It will escape the input
// string, expand any shell symbols (such as '~') and resolve any environment
// variables.
func Expand(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	p := syntax.NewParser()
	word, err := p.Document(strings.NewReader(s))
	if err != nil {
		return "", err
	}
	cfg := &expand.Config{
		Env:      expand.FuncEnviron(os.Getenv),
		ReadDir2: os.ReadDir,
		GlobStar: true,
	}
	return expand.Literal(cfg, word)
}

type FileInfo struct {
	Path    string
	ModTime time.Time
}

func SkipHidden(path string) bool {
	// Check for hidden files (starting with a dot)
	base := filepath.Base(path)
	if base != "." && strings.HasPrefix(base, ".") {
		return true
	}

	commonIgnoredDirs := map[string]bool{
		".rush":            true,
		"node_modules":     true,
		"vendor":           true,
		"dist":             true,
		"build":            true,
		"target":           true,
		".git":             true,
		".idea":            true,
		".vscode":          true,
		"__pycache__":      true,
		"bin":              true,
		"obj":              true,
		"out":              true,
		"coverage":         true,
		"logs":             true,
		"generated":        true,
		"bower_components": true,
		"jspm_packages":    true,
	}

	parts := strings.SplitSeq(path, string(os.PathSeparator))
	for part := range parts {
		if commonIgnoredDirs[part] {
			return true
		}
	}
	return false
}

// FastGlobWalker provides gitignore-aware file walking with fastwalk
// It uses hierarchical ignore checking like git does, checking .gitignore/.rushignore
// files in each directory from the root to the target path.
type FastGlobWalker struct {
	directoryLister *directoryLister
}

func NewFastGlobWalker(searchPath string) *FastGlobWalker {
	return &FastGlobWalker{
		directoryLister: NewDirectoryLister(searchPath),
	}
}

// NewFastGlobWalkerFS creates a gitignore-aware walker rooted at an fs.FS.
// The FS may be an os.Root filesystem, so traversal remains anchored.
func NewFastGlobWalkerFS(fsys fs.FS) *FastGlobWalker {
	return &FastGlobWalker{directoryLister: newDirectoryListerFS(fsys)}
}

func newFastGlobWalkerFSAt(fsys fs.FS, rootPath string) *FastGlobWalker {
	return &FastGlobWalker{directoryLister: newDirectoryListerFSAt(fsys, rootPath)}
}

// NewFastGlobWalkerFSAt creates an anchored walker whose ignore root is start.
func NewFastGlobWalkerFSAt(fsys fs.FS, start string) *FastGlobWalker {
	return newFastGlobWalkerFSAt(fsys, filepath.ToSlash(start))
}

// ShouldSkip checks if a file path should be skipped based on hierarchical gitignore,
// rushignore, and hidden file rules.
func (w *FastGlobWalker) ShouldSkip(path string) bool {
	return w.directoryLister.shouldIgnore(path, nil, false)
}

// ShouldSkipDir checks if a directory path should be skipped based on hierarchical
// gitignore, rushignore, and hidden file rules.
func (w *FastGlobWalker) ShouldSkipDir(path string) bool {
	return w.directoryLister.shouldIgnore(path, nil, true)
}

// Glob globs files.
//
// Does not respect gitignore.
func Glob(pattern string, cwd string, limit int) ([]string, bool, error) {
	return globWithDoubleStar(pattern, cwd, limit, false)
}

// GlobGitignoreAware globs files respecting gitignore.
func GlobGitignoreAware(pattern string, cwd string, limit int) ([]string, bool, error) {
	return globWithDoubleStar(pattern, cwd, limit, true)
}

// GlobGitignoreAwareNoFollow is the authorization-safe variant for callers
// that must never traverse directory symlinks.
func GlobGitignoreAwareNoFollow(pattern string, cwd string, limit int) ([]string, bool, error) {
	return globWithDoubleStarFollow(pattern, cwd, limit, true, false)
}

// GlobGitignoreAwareFS is the anchored counterpart of GlobGitignoreAware.
func GlobGitignoreAwareFS(fsys fs.FS, start, displayRoot, pattern string, limit int) ([]string, bool, error) {
	pattern = filepath.ToSlash(pattern)
	walker := newFastGlobWalkerFSAt(fsys, filepath.ToSlash(start))
	found := csync.NewSlice[FileInfo]()
	err := fs.WalkDir(fsys, filepath.ToSlash(start), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		isDir := d.IsDir()
		if isDir {
			if walker.ShouldSkipDir(path) {
				return fs.SkipDir
			}
		} else if walker.ShouldSkip(path) {
			return nil
		}
		relPath := path
		if start != "." {
			relPath, _ = filepath.Rel(start, path)
		}
		relPath = filepath.ToSlash(relPath)
		matched, matchErr := doublestar.Match(pattern, relPath)
		if matchErr != nil || !matched {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		displayPath := path
		if start != "." {
			displayPath, _ = filepath.Rel(start, path)
		}
		found.Append(FileInfo{Path: filepath.Join(displayRoot, filepath.FromSlash(displayPath)), ModTime: info.ModTime()})
		if limit > 0 && found.Len() >= limit*2 {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return nil, false, fmt.Errorf("fs walk error: %w", err)
	}
	matches := slices.SortedFunc(found.Seq(), func(a, b FileInfo) int { return b.ModTime.Compare(a.ModTime) })
	matches, truncated := truncate(matches, limit)
	results := make([]string, len(matches))
	for i, match := range matches {
		results[i] = match.Path
	}
	return results, truncated || errors.Is(err, fs.SkipAll), nil
}

func globWithDoubleStar(pattern, searchPath string, limit int, gitignore bool) ([]string, bool, error) {
	return globWithDoubleStarFollow(pattern, searchPath, limit, gitignore, true)
}

func globWithDoubleStarFollow(pattern, searchPath string, limit int, gitignore, follow bool) ([]string, bool, error) {
	// Normalize pattern to forward slashes on Windows so their config can use
	// backslashes
	pattern = filepath.ToSlash(pattern)

	walker := NewFastGlobWalker(searchPath)
	found := csync.NewSlice[FileInfo]()
	conf := fastwalk.Config{
		Follow:  follow,
		ToSlash: fastwalk.DefaultToSlash(),
		Sort:    fastwalk.SortFilesFirst,
	}
	err := fastwalk.Walk(&conf, searchPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // Skip files we can't access
		}

		isDir := d.IsDir()
		if isDir {
			if gitignore && walker.ShouldSkipDir(path) {
				return filepath.SkipDir
			}
		} else {
			if gitignore && walker.ShouldSkip(path) {
				return nil
			}
		}

		relPath, err := filepath.Rel(searchPath, path)
		if err != nil {
			relPath = path
		}

		// Normalize separators to forward slashes
		relPath = filepath.ToSlash(relPath)

		// Check if path matches the pattern
		matched, err := doublestar.Match(pattern, relPath)
		if err != nil || !matched {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return nil
		}

		// filepath.Clean, not the raw walk path: the walker joins onto
		// searchPath with whatever separators the caller passed, so a cwd
		// carrying forward slashes on Windows (a TMPDIR set that way, for
		// instance) would yield "D:/a/b.txt" here while the anchored
		// GlobGitignoreAwareFS counterpart yields "D:\a\b.txt" for the same
		// file -- it builds its result with filepath.Join. Two functions
		// documented as counterparts must not disagree about what a path
		// looks like. On Linux and macOS this is a no-op.
		found.Append(FileInfo{Path: filepath.Clean(path), ModTime: info.ModTime()})
		if limit > 0 && found.Len() >= limit*2 { // NOTE: why x2?
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return nil, false, fmt.Errorf("fastwalk error: %w", err)
	}

	matches := slices.SortedFunc(found.Seq(), func(a, b FileInfo) int {
		return b.ModTime.Compare(a.ModTime)
	})
	matches, truncated := truncate(matches, limit)

	results := make([]string, len(matches))
	for i, m := range matches {
		results[i] = m.Path
	}
	return results, truncated || errors.Is(err, filepath.SkipAll), nil
}

// ShouldExcludeFile checks if a file should be excluded from processing
// based on common patterns and ignore rules.
func ShouldExcludeFile(rootPath, filePath string) bool {
	info, err := os.Stat(filePath)
	isDir := err == nil && info.IsDir()
	return NewDirectoryLister(rootPath).
		shouldIgnore(filePath, nil, isDir)
}

func PrettyPath(path string) string {
	return home.Short(path)
}

func DirTrim(pwd string, lim int) string {
	var (
		out string
		sep = string(filepath.Separator)
	)
	dirs := strings.Split(pwd, sep)
	if lim > len(dirs)-1 || lim <= 0 {
		return pwd
	}
	for i := len(dirs) - 1; i > 0; i-- {
		out = sep + out
		if i == len(dirs)-1 {
			out = dirs[i]
		} else if i >= len(dirs)-lim {
			// Keep the first grapheme cluster, not the first byte: CJK,
			// combining marks, and emoji can span multiple bytes and runes,
			// so a byte or single rune would render the wrong character.
			first, _ := ansi.FirstGraphemeCluster(dirs[i], ansi.GraphemeWidth)
			out = first + out
		} else {
			out = "..." + out
			break
		}
	}
	out = filepath.Join("~", out)
	return out
}

// PathOrPrefix returns the prefix if the path starts with it, or falls back to
// the path otherwise.
func PathOrPrefix(path, prefix string) string {
	if HasPrefix(path, prefix) {
		return prefix
	}
	return path
}

// HasPrefix checks if the given path starts with the specified prefix.
// Uses filepath.Rel to determine if path is within prefix.
func HasPrefix(path, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	if err != nil {
		return false
	}
	// If path is within prefix, Rel will not return a path starting with ".."
	return !strings.HasPrefix(rel, "..")
}

// ToUnixLineEndings converts Windows line endings (CRLF) to Unix line endings (LF).
func ToUnixLineEndings(content string) (string, bool) {
	if strings.Contains(content, "\r\n") {
		return strings.ReplaceAll(content, "\r\n", "\n"), true
	}
	return content, false
}

// ToWindowsLineEndings converts Unix line endings (LF) to Windows line endings (CRLF).
func ToWindowsLineEndings(content string) (string, bool) {
	if !strings.Contains(content, "\r\n") {
		return strings.ReplaceAll(content, "\n", "\r\n"), true
	}
	return content, false
}

func truncate[T any](input []T, limit int) ([]T, bool) {
	if limit > 0 && len(input) > limit {
		return input[:limit], true
	}
	return input, false
}
