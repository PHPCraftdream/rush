package tools

// fakeDisk is the shared in-memory DiskProvider used by every test in this
// file: it never touches the real filesystem (backed entirely by maps) and
// records every call it receives, so a test can prove a tool went through
// the injected provider instead of quietly falling back to the real disk.
// Every test in this file pairs a fakeDisk with a workingDir that does NOT
// exist on the real filesystem (a subdirectory of t.TempDir() that is
// never actually created) and asserts the real t.TempDir() stays empty
// afterwards — a missed redirect would either fail outright (the real
// directory does not exist) or leave a stray real file behind, either of
// which fails the test loudly instead of silently passing against a real
// temp dir.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// fakeFileInfo is a minimal fs.FileInfo: only IsDir, ModTime and
// Mode().IsRegular() are consumed anywhere in the fs_* family (see
// DiskProvider.Stat's doc comment), so Size and Sys stay zero values.
type fakeFileInfo struct {
	name    string
	isDir   bool
	modTime time.Time
}

func (fi fakeFileInfo) Name() string { return fi.name }
func (fi fakeFileInfo) Size() int64  { return 0 }
func (fi fakeFileInfo) Mode() fs.FileMode {
	if fi.isDir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
func (fi fakeFileInfo) ModTime() time.Time { return fi.modTime }
func (fi fakeFileInfo) IsDir() bool        { return fi.isDir }
func (fi fakeFileInfo) Sys() any           { return nil }

// fakeDisk is an in-memory DiskProvider for the fs_* tool tests.
type fakeDisk struct {
	mu    sync.Mutex
	files map[string]string // absolute path -> content
	dirs  map[string]bool
	calls []string // "Stat:/abs/p", "WriteFile:/abs/p", ...

	// symlinks lets a test make EvalSymlinks resolve a path somewhere
	// OTHER than itself, so a test can prove resolveScopedPath actually
	// consulted the injected provider rather than the real
	// filepath.EvalSymlinks (which would never do this).
	symlinks map[string]string

	// searchLines is returned verbatim by Search; searchCalls counts how
	// many times Search was invoked.
	searchLines []SearchLine
	searchCalls int

	// listResult / findResult are returned verbatim by List / Find.
	listResult ListResult
	findResult FindResult
}

var _ DiskProvider = (*fakeDisk)(nil)

func newFakeDisk() *fakeDisk {
	return &fakeDisk{
		files: make(map[string]string),
		dirs:  make(map[string]bool),
	}
}

func (f *fakeDisk) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// Calls returns every call this fake received, in order.
func (f *fakeDisk) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// SearchCalls returns how many times Search was invoked.
func (f *fakeDisk) SearchCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searchCalls
}

// FileContent returns the fake's stored content for path (empty string if
// absent), for tests to assert what a write actually produced.
func (f *fakeDisk) FileContent(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files[filepath.Clean(path)]
}

// HasFile reports whether path currently exists in the fake.
func (f *fakeDisk) HasFile(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.files[filepath.Clean(path)]
	return ok
}

// putFile registers path as an existing file with content, and marks
// every ancestor directory up to the root as existing too, mirroring a
// real filesystem where a file's parents always exist.
func (f *fakeDisk) putFile(path, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path = filepath.Clean(path)
	f.files[path] = content
	for dir := filepath.Dir(path); ; {
		f.dirs[dir] = true
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
}

// putDir registers path as an existing directory.
func (f *fakeDisk) putDir(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[filepath.Clean(path)] = true
}

func (f *fakeDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	f.record("Stat:" + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	name = filepath.Clean(name)
	if _, ok := f.files[name]; ok {
		return fakeFileInfo{name: filepath.Base(name), modTime: time.Now()}, nil
	}
	if f.dirs[name] {
		return fakeFileInfo{name: filepath.Base(name), isDir: true, modTime: time.Now()}, nil
	}
	return nil, fmt.Errorf("fakeDisk: stat %s: %w", name, fs.ErrNotExist)
}

func (f *fakeDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	f.record("EvalSymlinks:" + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	name = filepath.Clean(name)
	if to, ok := f.symlinks[name]; ok {
		return to, nil
	}
	return name, nil
}

func (f *fakeDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	f.record("Open:" + name)
	f.mu.Lock()
	content, ok := f.files[filepath.Clean(name)]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fakeDisk: open %s: %w", name, fs.ErrNotExist)
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (f *fakeDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	f.record("ReadFile:" + name)
	f.mu.Lock()
	content, ok := f.files[filepath.Clean(name)]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("fakeDisk: read %s: %w", name, fs.ErrNotExist)
	}
	return []byte(content), nil
}

func (f *fakeDisk) MkdirAll(_ context.Context, dir string, _ fs.FileMode) error {
	f.record("MkdirAll:" + dir)
	f.putDir(dir)
	return nil
}

func (f *fakeDisk) WriteFile(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	f.record("WriteFile:" + name)
	f.putFile(name, string(data))
	return nil
}

func (f *fakeDisk) Remove(_ context.Context, name string) error {
	f.record("Remove:" + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	name = filepath.Clean(name)
	if _, ok := f.files[name]; !ok {
		return fmt.Errorf("fakeDisk: remove %s: %w", name, fs.ErrNotExist)
	}
	delete(f.files, name)
	return nil
}

func (f *fakeDisk) List(_ context.Context, req ListRequest) (ListResult, error) {
	f.record("List:" + req.Dir)
	return f.listResult, nil
}

func (f *fakeDisk) Find(_ context.Context, req FindRequest) (FindResult, error) {
	f.record("Find:" + req.Dir)
	return f.findResult, nil
}

func (f *fakeDisk) Search(_ context.Context, req SearchRequest) (DiskSearchResult, error) {
	f.mu.Lock()
	f.searchCalls++
	f.mu.Unlock()
	f.record("Search:" + req.Dir)
	return DiskSearchResult{Lines: f.searchLines}, nil
}

// fakeReadTracker is a filetracker.Service whose LastReadTime actually
// reflects RecordRead calls (unlike mockEditFileTracker/
// mockFileTrackerService elsewhere in this package, which return a fixed
// value regardless of what was recorded) — needed to prove the causal
// link between fs_read's RecordRead call and fs_replace/fs_write_lines's
// read-before-write gate.
type fakeReadTracker struct {
	mu    sync.Mutex
	reads map[string]time.Time
}

func newFakeReadTracker() *fakeReadTracker {
	return &fakeReadTracker{reads: make(map[string]time.Time)}
}

func (f *fakeReadTracker) RecordRead(_ context.Context, _ string, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads[filepath.Clean(path)] = time.Now()
}

func (f *fakeReadTracker) LastReadTime(_ context.Context, _ string, path string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads[filepath.Clean(path)]
}

func (f *fakeReadTracker) ListReadFiles(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

var _ filetracker.Service = (*fakeReadTracker)(nil)

// requireRealDirEmpty asserts dir (a real t.TempDir()) has no entries,
// the standing proof that a test's disk I/O never escaped the fake.
func requireRealDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "the real directory must stay untouched")
}

func TestResolveScopedPath_UsesInjectedDiskProvider(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	filePath := filepath.Join(workingDir, "file.txt")
	disk.putFile(filePath, "hello")

	// A path that does not exist on the real disk (workingDir is never
	// created for real) resolves successfully through the fake.
	abs, err := resolveScopedPath(context.Background(), disk, workingDir, "file.txt")
	require.NoError(t, err)
	require.Equal(t, filePath, abs)

	// A fake symlink is followed: redirect workingDir itself elsewhere.
	// If resolveScopedPath had used the REAL filepath.EvalSymlinks
	// instead, it would return workingDir unchanged (no such symlink
	// exists for real), so a divergent result proves the fake was used.
	redirected := filepath.Join(tmp, "elsewhere")
	disk.mu.Lock()
	disk.symlinks = map[string]string{filepath.Clean(workingDir): redirected}
	disk.mu.Unlock()

	abs, err = resolveScopedPath(context.Background(), disk, workingDir, "new.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(redirected, "new.txt"), abs)

	requireRealDirEmpty(t, tmp)
}

func TestFSReadUsesInjectedDiskProvider(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	filePath := filepath.Join(workingDir, "file.txt")
	disk.putFile(filePath, "line one\nline two\nline three")

	scope := fsBatchTestScope(t, workingDir, permission.FileOpRead)
	tool := NewFSReadTool(scope, mockFileTrackerService{}, workingDir, disk)

	raw, err := json.Marshal(FSReadParams{Items: []FSReadItem{{Path: "file.txt"}}})
	require.NoError(t, err)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-fake-read")
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: FSReadToolName, Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "line one")
	require.Contains(t, resp.Content, "line three")
	require.Contains(t, disk.Calls(), "Open:"+filePath)

	requireRealDirEmpty(t, tmp)
}

func TestFSListUsesInjectedDiskProvider(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	disk.listResult = ListResult{
		Entries: []string{
			filepath.Join(workingDir, "a.txt"),
			filepath.Join(workingDir, "sub") + string(filepath.Separator),
			filepath.Join(workingDir, "sub", "b.txt"),
		},
	}

	scope := fsBatchTestScope(t, workingDir, permission.FileOpList)
	tool := NewFSListTool(scope, workingDir, config.ToolLs{}, disk)

	raw, err := json.Marshal(FSListParams{Items: []FSListItem{{Path: "."}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "a.txt")
	require.Contains(t, resp.Content, "sub/")
	require.Contains(t, resp.Content, "b.txt")
	require.Contains(t, disk.Calls(), "List:"+workingDir)

	requireRealDirEmpty(t, tmp)
}

func TestFSFindUsesInjectedDiskProvider(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	disk.findResult = FindResult{Paths: []string{filepath.Join(workingDir, "keep.txt")}}

	scope := fsBatchTestScope(t, workingDir, permission.FileOpFind)
	tool := NewFSFindTool(scope, workingDir, disk)

	raw, err := json.Marshal(FSFindParams{Items: []FSFindItem{{Pattern: "**/*.txt"}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "keep.txt")
	require.Contains(t, disk.Calls(), "Find:"+workingDir)

	requireRealDirEmpty(t, tmp)
}

// TestFSGrepUsesInjectedDiskProviderAndNeverSpawnsRipgrep is also the
// "no rg subprocess" proof: with an injected DiskProvider, fsGrepRunItem
// calls disk.Search exclusively (see fs_grep.go) — the ripgrep dispatch
// (fsGrepSearchContext / searchWithRipgrepContext) is only ever reached
// from OSDisk.Search, which is never invoked here. The fake's "a.txt" is
// deliberately never registered as a real file in the fake, so the
// rendered text can ONLY have come from the injected SearchLine, not
// from re-reading file content through any code path.
func TestFSGrepUsesInjectedDiskProviderAndNeverSpawnsRipgrep(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	disk.searchLines = []SearchLine{
		{Path: filepath.Join(workingDir, "a.txt"), Line: 1, Text: "needle only the fake knows about", Hit: true},
	}

	scope := fsBatchTestScope(t, workingDir, permission.FileOpGrep)
	tool := NewFSGrepTool(workingDir, scope, disk)

	raw, err := json.Marshal(FSGrepParams{Items: []FSGrepItem{{Pattern: "needle", Path: workingDir}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "needle only the fake knows about")
	require.Equal(t, 1, disk.SearchCalls(), "Search must be called exactly once")

	requireRealDirEmpty(t, tmp)
}

func TestFSWriteUsesInjectedDiskProvider(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	// fs_write's execute path also scope-checks and stats missing
	// ancestors up to the first EXISTING one (see fs_write.go); tmp is a
	// real ancestor of workingDir, so the fake must agree it exists too.
	disk.putDir(tmp)
	disk.putDir(workingDir)

	scope := fsWriteTestScope(t, workingDir)
	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, workingDir, disk)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-fake-write")
	raw, err := json.Marshal(FSWriteParams{Items: []FSWriteItem{{Path: "new.txt", Content: "hello"}}})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: FSWriteToolName, Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "hello", disk.FileContent(filepath.Join(workingDir, "new.txt")))

	requireRealDirEmpty(t, tmp)
}

func TestFSReplaceUsesInjectedDiskProvider(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	filePath := filepath.Join(workingDir, "doc.txt")
	disk.putFile(filePath, "alpha\nbeta\n")

	tracker := newFakeReadTracker()
	sessionID := "sess-fake-replace"
	tracker.RecordRead(context.Background(), sessionID, filePath)

	scope := fsWriteTestScope(t, workingDir, permission.FolderScopeEntry{
		Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace, permission.FileOpRead},
	})
	tool := NewFSReplaceTool(scope, &mockPermissionService{}, &mockHistoryService{}, tracker, workingDir, disk)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, sessionID)
	raw, err := json.Marshal(FSReplaceParams{Items: []FSReplaceItem{{Path: "doc.txt", OldString: "beta", NewString: "BETA"}}})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: FSReplaceToolName, Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "alpha\nBETA\n", disk.FileContent(filePath))

	requireRealDirEmpty(t, tmp)
}

func TestFSWriteLinesUsesInjectedDiskProvider(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	filePath := filepath.Join(workingDir, "lines.txt")
	disk.putFile(filePath, "a\nb\nc\n")

	tracker := newFakeReadTracker()
	sessionID := "sess-fake-write-lines"
	tracker.RecordRead(context.Background(), sessionID, filePath)

	scope := fsWriteTestScope(t, workingDir, permission.FolderScopeEntry{
		Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead},
	})
	tool := NewFSWriteLinesTool(scope, &mockPermissionService{}, &mockHistoryService{}, tracker, workingDir, disk)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, sessionID)
	raw, err := json.Marshal(FSWriteLinesParams{Items: []FSWriteLinesItem{{Path: "lines.txt", StartLine: 2, EndLine: 2, Content: "B"}}})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: FSWriteLinesToolName, Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "a\nB\nc\n", disk.FileContent(filePath))

	requireRealDirEmpty(t, tmp)
}

func TestFSDeleteUsesInjectedDiskProvider(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	filePath := filepath.Join(workingDir, "gone.txt")
	disk.putFile(filePath, "x")

	scope := fsWriteTestScope(t, workingDir, permission.FolderScopeEntry{
		Dir: ".", Ops: []permission.FileOp{permission.FileOpDelete},
	})
	tool := NewFSDeleteTool(scope, &mockPermissionService{}, workingDir, disk)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-fake-delete")
	raw, err := json.Marshal(FSDeleteParams{Items: []FSDeleteItem{{Path: "gone.txt"}}})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: FSDeleteToolName, Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.False(t, disk.HasFile(filePath))
	require.Contains(t, disk.Calls(), "Remove:"+filePath)

	requireRealDirEmpty(t, tmp)
}

// TestFSToolsNilDiskProviderStillUsesTheRealDisk is a table over all
// eight constructors proving nil still means the real disk — the
// existing per-tool tests gaining a trailing nil argument are the
// broader version of this same proof.
func TestFSToolsNilDiskProviderStillUsesTheRealDisk(t *testing.T) {
	t.Parallel()

	t.Run("fs_read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("real content"), 0o644))
		tool := NewFSReadTool(fsBatchTestScope(t, dir, permission.FileOpRead), mockFileTrackerService{}, dir, nil)
		raw, err := json.Marshal(FSReadParams{Items: []FSReadItem{{Path: "real.txt"}}})
		require.NoError(t, err)
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c", Input: string(raw)})
		require.NoError(t, err)
		require.Contains(t, resp.Content, "real content")
	})

	t.Run("fs_list", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("x"), 0o644))
		tool := NewFSListTool(fsBatchTestScope(t, dir, permission.FileOpList), dir, config.ToolLs{}, nil)
		raw, err := json.Marshal(FSListParams{Items: []FSListItem{{Path: "."}}})
		require.NoError(t, err)
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c", Input: string(raw)})
		require.NoError(t, err)
		require.Contains(t, resp.Content, "real.txt")
	})

	t.Run("fs_find", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("x"), 0o644))
		tool := NewFSFindTool(fsBatchTestScope(t, dir, permission.FileOpFind), dir, nil)
		raw, err := json.Marshal(FSFindParams{Items: []FSFindItem{{Pattern: "**/*.txt"}}})
		require.NoError(t, err)
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c", Input: string(raw)})
		require.NoError(t, err)
		require.Contains(t, resp.Content, "real.txt")
	})

	t.Run("fs_grep", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("needle\n"), 0o644))
		tool := NewFSGrepTool(dir, fsBatchTestScope(t, dir, permission.FileOpGrep), nil)
		raw, err := json.Marshal(FSGrepParams{Items: []FSGrepItem{{Pattern: "needle", Path: dir}}})
		require.NoError(t, err)
		resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c", Input: string(raw)})
		require.NoError(t, err)
		require.Contains(t, resp.Content, "needle")
	})

	t.Run("fs_write", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		tool := NewFSWriteTool(fsWriteTestScope(t, dir), &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-nil-write")
		raw, err := json.Marshal(FSWriteParams{Items: []FSWriteItem{{Path: "real.txt", Content: "hello"}}})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: FSWriteToolName, Input: string(raw)})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		content, err := os.ReadFile(filepath.Join(dir, "real.txt"))
		require.NoError(t, err)
		require.Equal(t, "hello", string(content))
	})

	t.Run("fs_replace", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("alpha\nbeta\n"), 0o644))
		tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
		scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{
			Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace, permission.FileOpRead},
		})
		tool := NewFSReplaceTool(scope, &mockPermissionService{}, &mockHistoryService{}, tracker, dir, nil)
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-nil-replace")
		raw, err := json.Marshal(FSReplaceParams{Items: []FSReplaceItem{{Path: "real.txt", OldString: "beta", NewString: "BETA"}}})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: FSReplaceToolName, Input: string(raw)})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		content, err := os.ReadFile(filepath.Join(dir, "real.txt"))
		require.NoError(t, err)
		require.Equal(t, "alpha\nBETA\n", string(content))
	})

	t.Run("fs_write_lines", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("a\nb\nc\n"), 0o644))
		tracker := &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}
		scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{
			Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead},
		})
		tool := NewFSWriteLinesTool(scope, &mockPermissionService{}, &mockHistoryService{}, tracker, dir, nil)
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-nil-write-lines")
		raw, err := json.Marshal(FSWriteLinesParams{Items: []FSWriteLinesItem{{Path: "real.txt", StartLine: 2, EndLine: 2, Content: "B"}}})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: FSWriteLinesToolName, Input: string(raw)})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		content, err := os.ReadFile(filepath.Join(dir, "real.txt"))
		require.NoError(t, err)
		require.Equal(t, "a\nB\nc\n", string(content))
	})

	t.Run("fs_delete", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "real.txt"), []byte("x"), 0o644))
		scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{
			Dir: ".", Ops: []permission.FileOp{permission.FileOpDelete},
		})
		tool := NewFSDeleteTool(scope, &mockPermissionService{}, dir, nil)
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-nil-delete")
		raw, err := json.Marshal(FSDeleteParams{Items: []FSDeleteItem{{Path: "real.txt"}}})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c", Name: FSDeleteToolName, Input: string(raw)})
		require.NoError(t, err)
		require.False(t, resp.IsError)
		_, statErr := os.Stat(filepath.Join(dir, "real.txt"))
		require.True(t, os.IsNotExist(statErr))
	})
}

// TestFSWriteHistoryAndFileTrackerRecordAgainstRealServices pins a
// deliberate decision: only the fs_* family's DISK I/O is redirected by
// DiskProvider. History and filetracker are explicitly NOT redirected —
// they keep recording against whatever service the tool was constructed
// with, real or mock, regardless of where the file content itself lives.
// This uses genuine filetracker.Service/history.Service instances backed
// by a real (in-memory-path) sqlite database, the same pattern as
// internal/filetracker/service_test.go, so the proof is against the real
// production services, not a test double.
func TestFSWriteHistoryAndFileTrackerRecordAgainstRealServices(t *testing.T) {
	t.Parallel()

	dbDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dbDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dbDir) })
	q := db.New(conn)

	sessionID := "sess-real-services"
	_, err = q.CreateSession(t.Context(), db.CreateSessionParams{ID: sessionID, Title: "real services test"})
	require.NoError(t, err)

	realFiletracker := filetracker.NewService(q)
	realHistory := history.NewService(q, conn)

	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(tmp)
	disk.putDir(workingDir)

	scope := fsWriteTestScope(t, workingDir)
	tool := NewFSWriteTool(scope, &mockPermissionService{}, realHistory, realFiletracker, workingDir, disk)

	ctx := context.WithValue(t.Context(), SessionIDContextKey, sessionID)
	filePath := filepath.Join(workingDir, "tracked.txt")
	raw, err := json.Marshal(FSWriteParams{Items: []FSWriteItem{{Path: "tracked.txt", Content: "hello"}}})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: FSWriteToolName, Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	// The write itself landed only in the fake disk.
	require.Equal(t, "hello", disk.FileContent(filePath))
	requireRealDirEmpty(t, tmp)

	// But history and filetracker recorded against the REAL services.
	lastRead := realFiletracker.LastReadTime(ctx, sessionID, filePath)
	require.False(t, lastRead.IsZero(), "filetracker must have recorded the write against the real service")

	file, err := realHistory.GetByPathAndSession(ctx, filePath, sessionID)
	require.NoError(t, err)
	require.Equal(t, "hello", file.Content)
}

// TestFSReadRecordsReadSoFSReplaceCanSucceedWithoutAPriorWrite pins the
// RecordRead fix bundled into this task: before it, fs_read never called
// filetracker.RecordRead, so fs_replace/fs_write_lines could only ever
// succeed on a file fs_write had already written in the same session,
// never on a file the model only read. Uses fakeReadTracker (not
// mockEditFileTracker/mockFileTrackerService, whose LastReadTime ignores
// RecordRead) so the causal link is actually exercised.
func TestFSReadRecordsReadSoFSReplaceCanSucceedWithoutAPriorWrite(t *testing.T) {
	t.Parallel()
	// t.TempDir() is not guaranteed to already be canonical on every CI
	// runner image (macOS's /var -> /private/var; a redirected Windows
	// temp drive). fsBatchTestScope/fsWriteTestScope resolve workingDir's
	// longest real-disk-existing prefix through the REAL disk, while the
	// tool resolves item paths through the FAKE disk, which knows nothing
	// about a real symlink/junction on tmp -- a raw t.TempDir() would put
	// the scope root and every item path in different namespaces. Same
	// host-topology class already fixed in 73878311 / b253bb70.
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	filePath := filepath.Join(workingDir, "doc.txt")
	disk.putFile(filePath, "alpha\nbeta\n")

	tracker := newFakeReadTracker()
	sessionID := "sess-read-then-replace"
	ctx := context.WithValue(context.Background(), SessionIDContextKey, sessionID)

	replaceScope := fsWriteTestScope(t, workingDir, permission.FolderScopeEntry{
		Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace, permission.FileOpRead},
	})
	replaceTool := NewFSReplaceTool(replaceScope, &mockPermissionService{}, &mockHistoryService{}, tracker, workingDir, disk)
	replaceRaw, err := json.Marshal(FSReplaceParams{Items: []FSReplaceItem{{Path: "doc.txt", OldString: "beta", NewString: "BETA"}}})
	require.NoError(t, err)

	// Before any read, fs_replace must refuse: LastReadTime is zero.
	resp, err := replaceTool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: FSReplaceToolName, Input: string(replaceRaw)})
	require.NoError(t, err)
	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "read the file before")
	require.Equal(t, "alpha\nbeta\n", disk.FileContent(filePath), "the refused replace must not have touched the file")

	// A bare fs_read of the same file — fs_write is never called.
	readScope := fsBatchTestScope(t, workingDir, permission.FileOpRead)
	readTool := NewFSReadTool(readScope, tracker, workingDir, disk)
	readRaw, err := json.Marshal(FSReadParams{Items: []FSReadItem{{Path: "doc.txt"}}})
	require.NoError(t, err)
	resp, err = readTool.Run(ctx, fantasy.ToolCall{ID: "c2", Name: FSReadToolName, Input: string(readRaw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	// Now fs_replace succeeds on the very same tracker.
	resp, err = replaceTool.Run(ctx, fantasy.ToolCall{ID: "c3", Name: FSReplaceToolName, Input: string(replaceRaw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)
	require.Equal(t, "alpha\nBETA\n", disk.FileContent(filePath))

	requireRealDirEmpty(t, tmp)
}
