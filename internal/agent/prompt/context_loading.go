package prompt

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filepathext"
	"github.com/PHPCraftdream/rush/internal/kit/fs/home"
)

const (
	// These limits bound prompt construction before template rendering and
	// apply to project and global context together.
	maxContextFileBytes = 64 * 1024
	maxContextFiles     = 128
	maxContextBytes     = 512 * 1024
	maxContextEntries   = 4096
	maxContextDepth     = 64
	maxContextPathBytes = 16 * 1024
	contextReadDirChunk = 64
)

type contextBudget struct {
	files   int
	bytes   int
	entries int
}

type contextDirectory interface {
	ReadDir(n int) ([]os.DirEntry, error)
	Close() error
}

// contextFile is deliberately small so tests can provide a reader whose
// Close unblocks a pending Read. Implementations must honor that contract:
// cancellation closes the owned handle and joins the read/stat goroutine
// before returning, so no detached goroutine is permitted.
type contextFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type contextFileOpener func(path string) (contextFile, error)

func openContextFile(path string) (contextFile, error) {
	return os.Open(path)
}

type contextFileOpenerKey struct{}

type contextDirectoryOpener func(path string) (contextDirectory, error)

type contextDirectoryOpenerKey struct{}

type contextFileInspector func(path string) bool

type contextFileInspectorKey struct{}

func withContextFileOpener(ctx context.Context, opener contextFileOpener) context.Context {
	return context.WithValue(ctx, contextFileOpenerKey{}, opener)
}

func contextFileOpenerFrom(ctx context.Context) contextFileOpener {
	if opener, ok := ctx.Value(contextFileOpenerKey{}).(contextFileOpener); ok && opener != nil {
		return opener
	}
	return openContextFile
}

func withContextDirectoryOpener(ctx context.Context, opener contextDirectoryOpener) context.Context {
	return context.WithValue(ctx, contextDirectoryOpenerKey{}, opener)
}

func contextDirectoryOpenerFrom(ctx context.Context) contextDirectoryOpener {
	if opener, ok := ctx.Value(contextDirectoryOpenerKey{}).(contextDirectoryOpener); ok && opener != nil {
		return opener
	}
	return func(path string) (contextDirectory, error) { return os.Open(path) }
}

func withContextFileInspector(ctx context.Context, inspector contextFileInspector) context.Context {
	return context.WithValue(ctx, contextFileInspectorKey{}, inspector)
}

func contextFileInspectorFrom(ctx context.Context) contextFileInspector {
	if inspector, ok := ctx.Value(contextFileInspectorKey{}).(contextFileInspector); ok && inspector != nil {
		return inspector
	}
	return absoluteContextFileAllowed
}

// readContextChunk joins the reader goroutine before returning. Closing the
// owned handle on cancellation makes the cancellation path bounded without
// leaving a detached goroutine behind.
func readContextChunk(ctx context.Context, f contextFile, dst []byte) (int, error) {
	type result struct {
		n   int
		err error
	}
	readDone := make(chan result, 1)
	go func() {
		n, err := f.Read(dst)
		readDone <- result{n: n, err: err}
	}()

	select {
	case result := <-readDone:
		return result.n, result.err
	case <-ctx.Done():
		_ = f.Close()
		<-readDone
		return 0, ctx.Err()
	}
}

func statContextFile(ctx context.Context, f contextFile) (os.FileInfo, error) {
	type result struct {
		info os.FileInfo
		err  error
	}
	statDone := make(chan result, 1)
	go func() {
		info, err := f.Stat()
		statDone <- result{info: info, err: err}
	}()

	select {
	case result := <-statDone:
		return result.info, result.err
	case <-ctx.Done():
		_ = f.Close()
		<-statDone
		return nil, ctx.Err()
	}
}

func processFile(filePath string) *ContextFile {
	budget := contextBudget{}
	return readContextFile(context.Background(), filePath, &budget)
}

func processContextPath(p string, store *config.ConfigStore) []ContextFile {
	return processContextPathWithBudget(context.Background(), p, store, &contextBudget{})
}

func processContextPathWithBudget(ctx context.Context, p string, store *config.ConfigStore, budget *contextBudget) []ContextFile {
	// Unreadable, nonregular, escaping, duplicate-by-path, and over-limit
	// files are skipped without partial content. Cancellation aborts Build
	// with ctx.Err instead of rendering a partial context set.
	if budget.files >= maxContextFiles || budget.bytes >= maxContextBytes {
		return nil
	}
	fullPath, trusted, err := contextPath(p, store.WorkingDir())
	if err != nil {
		slog.Debug("Skipping context path", "path", p, "error", err)
		return nil
	}
	if !trusted {
		return processRelativeContextPath(ctx, fullPath, store.WorkingDir(), budget)
	}

	if err := ctx.Err(); err != nil {
		return nil
	}
	if !budget.visit(fullPath) {
		return nil
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		return nil
	}
	targetInfo, err := os.Stat(fullPath)
	if err != nil {
		return nil
	}
	if targetInfo.IsDir() {
		if info.Mode()&os.ModeSymlink != 0 || isContextReparsePoint(fullPath) {
			return nil
		}
		dir, err := contextDirectoryOpenerFrom(ctx)(fullPath)
		if err != nil {
			return nil
		}
		return walkContextDirectory(ctx, dir, fullPath, fullPath, 0, budget, contextDirectoryOpenerFrom(ctx), contextFileOpenerFrom(ctx), contextFileInspectorFrom(ctx))
	}
	if !targetInfo.Mode().IsRegular() {
		return nil
	}
	return contextResult(readContextFileWithOpener(ctx, fullPath, budget, contextFileOpenerFrom(ctx)))
}

func processRelativeContextPath(ctx context.Context, fullPath, workingDir string, budget *contextBudget) []ContextFile {
	rootPath, err := filepath.Abs(workingDir)
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(rootPath, fullPath)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	if !budget.visit(fullPath) {
		return nil
	}
	root, err := openContextRoot(rootPath)
	if err != nil {
		return nil
	}
	defer root.Close()
	rel = filepath.ToSlash(rel)
	info, err := root.Lstat(rel)
	if err != nil {
		return nil
	}
	targetInfo, err := root.Stat(rel)
	if err != nil {
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if targetInfo.IsDir() {
			return nil
		}
	}
	if !targetInfo.IsDir() {
		if !targetInfo.Mode().IsRegular() {
			return nil
		}
		entry, err := root.Open(rel)
		if err != nil {
			return nil
		}
		return contextResult(readContextFileOpened(ctx, fullPath, entry, budget))
	}
	entry, err := root.Open(rel)
	if err != nil {
		return nil
	}
	return walkContextDirectory(ctx, entry, rel, fullPath, 0, budget, func(path string) (contextDirectory, error) {
		return root.Open(path)
	}, func(path string) (contextFile, error) {
		return root.Open(path)
	}, func(path string) bool { return rootedContextFileAllowed(root, path) })
}

func walkContextDirectory(
	ctx context.Context,
	dir contextDirectory,
	dirToken, displayDir string,
	depth int,
	budget *contextBudget,
	openDir func(string) (contextDirectory, error),
	openFile func(string) (contextFile, error),
	inspectFile func(string) bool,
) []ContextFile {
	defer dir.Close()
	if depth >= maxContextDepth {
		return nil
	}
	entries := make([]os.DirEntry, 0, min(contextReadDirChunk, maxContextEntries-budget.entries))
	for len(entries) < maxContextEntries-budget.entries {
		if err := ctx.Err(); err != nil {
			return nil
		}
		remaining := maxContextEntries - budget.entries - len(entries)
		if remaining <= 0 {
			break
		}
		chunkSize := min(contextReadDirChunk, remaining)
		chunk, err := dir.ReadDir(chunkSize)
		entries = append(entries, chunk...)
		if err != nil {
			if err != io.EOF {
				slog.Debug("Stopping context directory traversal", "path", displayDir, "error", err)
			}
			break
		}
		if len(chunk) == 0 {
			break
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var contexts []ContextFile
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil
		}
		path := joinContextToken(dirToken, entry.Name())
		displayPath := joinContextDisplay(displayDir, entry.Name())
		if !budget.visit(displayPath) {
			return contexts
		}
		if entry.IsDir() {
			if depth+1 >= maxContextDepth || isContextReparsePoint(displayPath) {
				continue
			}
			subdir, err := openDir(path)
			if err != nil {
				continue
			}
			contexts = append(contexts, walkContextDirectory(ctx, subdir, path, displayPath, depth+1, budget, openDir, openFile, inspectFile)...)
			continue
		}
		if !inspectFile(path) {
			continue
		}
		file, err := openFile(path)
		if err != nil {
			continue
		}
		if result := readContextFileOpened(ctx, displayPath, file, budget); result != nil {
			contexts = append(contexts, *result)
		}
	}
	return contexts
}

func absoluteContextFileAllowed(path string) bool {
	if _, err := os.Lstat(path); err != nil {
		return false
	}
	targetInfo, err := os.Stat(path)
	return err == nil && targetInfo.Mode().IsRegular()
}

func rootedContextFileAllowed(root *os.Root, path string) bool {
	if _, err := root.Lstat(path); err != nil {
		return false
	}
	targetInfo, err := root.Stat(path)
	return err == nil && targetInfo.Mode().IsRegular()
}

func (b *contextBudget) visit(path string) bool {
	if b.entries >= maxContextEntries {
		return false
	}
	b.entries++
	return len(path) <= maxContextPathBytes
}

func joinContextToken(dir, name string) string {
	if filepath.IsAbs(dir) {
		return filepath.Join(dir, name)
	}
	return filepath.ToSlash(filepath.Join(filepath.FromSlash(dir), name))
}

func joinContextDisplay(dir, name string) string {
	return filepath.Join(dir, name)
}

func contextResult(file *ContextFile) []ContextFile {
	if file == nil {
		return nil
	}
	return []ContextFile{*file}
}

func openContextRoot(path string) (*os.Root, error) {
	for attempt := 0; attempt < 3; attempt++ {
		rootInfo, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		root, err := os.OpenRoot(path)
		if err != nil {
			continue
		}
		openedInfo, err := root.Stat(".")
		if err == nil && os.SameFile(rootInfo, openedInfo) {
			return root, nil
		}
		_ = root.Close()
	}
	return nil, fmt.Errorf("working directory changed while opening context root")
}

func contextPath(path, workingDir string) (fullPath string, trusted bool, err error) {
	if filepathext.SmartIsAbs(path) {
		fullPath, err = filepath.Abs(path)
		return fullPath, true, err
	}
	if filepath.VolumeName(path) != "" || hasParentPathElement(path) {
		return "", false, fmt.Errorf("relative context path is not project-relative")
	}
	root, err := filepath.Abs(workingDir)
	if err != nil {
		return "", false, err
	}
	return filepath.Join(root, path), false, nil
}

func hasParentPathElement(path string) bool {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

func readContextFile(ctx context.Context, filePath string, budget *contextBudget) *ContextFile {
	return readContextFileWithOpener(ctx, filePath, budget, contextFileOpenerFrom(ctx))
}

func readContextFileWithOpener(ctx context.Context, filePath string, budget *contextBudget, opener contextFileOpener) *ContextFile {
	if err := ctx.Err(); err != nil || budget.files >= maxContextFiles {
		return nil
	}
	remaining := maxContextBytes - budget.bytes
	if remaining <= 0 {
		return nil
	}
	f, err := opener(filePath)
	if err != nil {
		return nil
	}
	return readContextFileOpened(ctx, filePath, f, budget)
}

func readContextFileOpened(ctx context.Context, filePath string, f contextFile, budget *contextBudget) *ContextFile {
	if err := ctx.Err(); err != nil || budget.files >= maxContextFiles {
		_ = f.Close()
		return nil
	}
	remaining := maxContextBytes - budget.bytes
	if remaining <= 0 {
		_ = f.Close()
		return nil
	}
	limit := min(maxContextFileBytes, remaining)
	defer f.Close()
	info, err := statContextFile(ctx, f)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > int64(limit) {
		return nil
	}

	data := make([]byte, limit+1)
	n := 0
	for n < len(data) {
		readN, readErr := readContextChunk(ctx, f, data[n:])
		n += readN
		if readErr != nil && readErr != io.EOF {
			return nil
		}
		if n > limit {
			return nil
		}
		if n > int(info.Size()) {
			return nil
		}
		if readErr == io.EOF {
			break
		}
		if readN == 0 {
			if n == int(info.Size()) {
				break
			}
			return nil
		}
	}
	if n > limit || n != int(info.Size()) {
		return nil
	}
	// A regular file can grow after Stat. Probe for one more byte when the
	// advertised size did not fill the bounded buffer.
	if n < len(data) && n == int(info.Size()) {
		readN, readErr := readContextChunk(ctx, f, data[n:n+1])
		if readN > 0 || (readErr != nil && readErr != io.EOF) {
			return nil
		}
	}
	if n > limit {
		return nil
	}
	budget.files++
	budget.bytes += n
	return &ContextFile{Path: filePath, Content: string(data[:n])}
}

// expandPath expands ~ and environment variables in file paths
func expandPath(path string, store *config.ConfigStore) string {
	path = home.Long(path)
	// Handle environment variable expansion using the same pattern as config
	if strings.HasPrefix(path, "$") {
		if expanded, err := store.Resolver().ResolveValue(path); err == nil {
			path = expanded
		}
	}

	return path
}

// loadContextFiles loads and deduplicates context files from a list of paths.
func loadContextFiles(paths []string, store *config.ConfigStore) map[string][]ContextFile {
	return loadContextFilesWithBudget(context.Background(), paths, store, &contextBudget{})
}

func loadContextFilesWithBudget(ctx context.Context, paths []string, store *config.ConfigStore, budget *contextBudget) map[string][]ContextFile {
	files := map[string][]ContextFile{}
	for _, pth := range paths {
		if ctx.Err() != nil {
			return files
		}
		expanded := expandPath(pth, store)
		pathKey := strings.ToLower(expanded)
		if _, ok := files[pathKey]; ok {
			continue
		}
		files[pathKey] = processContextPathWithBudget(ctx, expanded, store, budget)
	}
	return files
}

// flattenContextFiles collects a path-keyed context-file map into a single
// slice, sorted by path key for deterministic ordering — map iteration order
// in Go is randomized, and dedupeContextFiles below needs a stable "first
// occurrence wins" rule to produce the same result on every run.
func flattenContextFiles(byPath map[string][]ContextFile) []ContextFile {
	keys := make([]string, 0, len(byPath))
	for k := range byPath {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var out []ContextFile
	for _, k := range keys {
		out = append(out, byPath[k]...)
	}
	return out
}

// dedupeContextFiles drops files whose content is byte-identical to a file
// already kept. Popular coding agents each look for their own instruction
// file (CLAUDE.md, AGENTS.md, GEMINI.md, ...; see defaultContextPaths in
// internal/config/config.go), so a project that keeps several of them in
// sync — often literal copies or symlinks of one another — would otherwise
// have the same instructions injected into the prompt multiple times,
// wasting context for no benefit. The first occurrence, in the deterministic
// order produced by flattenContextFiles, wins; later duplicates are dropped
// entirely (not just blanked), so their <file path="..."> wrapper doesn't
// appear either.
func dedupeContextFiles(files []ContextFile) []ContextFile {
	seen := make(map[[sha256.Size]byte]bool, len(files))
	out := make([]ContextFile, 0, len(files))
	for _, f := range files {
		hash := sha256.Sum256([]byte(f.Content))
		if seen[hash] {
			slog.Debug("prompt: skipping duplicate context file content", "path", f.Path)
			continue
		}
		seen[hash] = true
		out = append(out, f)
	}
	return out
}
