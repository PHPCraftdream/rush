package prompt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func contextTestPrompt(t *testing.T, root string, project, global []string) string {
	t.Helper()
	cfg := &config.Config{Options: &config.Options{
		ContextPaths:       project,
		GlobalContextPaths: global,
	}}
	store := config.NewLibraryStore(cfg, root)
	p, err := NewPrompt("context-test", "{{range .ContextFiles}}{{.Content}}{{end}}{{range .GlobalContextFiles}}{{.Content}}{{end}}")
	require.NoError(t, err)
	got, err := p.Build(context.Background(), "", "", store, cfg, false)
	require.NoError(t, err)
	return got
}

func TestBuild_ContextRelativeSymlinkDoesNotReadOutside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	require.NoError(t, os.WriteFile(secret, []byte("never inject this secret"), 0o600))
	link := filepath.Join(root, "AGENTS.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := contextTestPrompt(t, root, []string{"AGENTS.md"}, nil)
	require.NotContains(t, got, "never inject this secret")
}

func TestBuild_ContextRelativeDirectorySymlinkDoesNotReadOutside(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret.md"), []byte("outside directory secret"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(root, "rules"), 0o700))
	if err := os.Symlink(outside, filepath.Join(root, "rules", "external")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got := contextTestPrompt(t, root, []string{"rules"}, nil)
	require.NotContains(t, got, "outside directory secret")
}

func TestBuild_ContextRelativeParentPathIsRejected(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "parent-secret.md")
	require.NoError(t, os.WriteFile(outside, []byte("parent secret"), 0o600))
	t.Cleanup(func() { _ = os.Remove(outside) })

	got := contextTestPrompt(t, root, []string{filepath.Join("..", filepath.Base(outside))}, nil)
	require.NotContains(t, got, "parent secret")
}

func TestBuild_ContextAbsoluteGlobalRegularFileWorks(t *testing.T) {
	root := t.TempDir()
	global := filepath.Join(t.TempDir(), "preferences.md")
	require.NoError(t, os.WriteFile(global, []byte("operator preference"), 0o600))

	got := contextTestPrompt(t, root, nil, []string{global})
	require.Contains(t, got, "operator preference")
}

func TestReadContextFile_LimitsAreExactAndShared(t *testing.T) {
	t.Run("per-file exact", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "exact.md")
		require.NoError(t, os.WriteFile(path, bytesOf('a', maxContextFileBytes), 0o600))
		budget := contextBudget{}
		require.NotNil(t, readContextFile(context.Background(), path, &budget))
		require.Equal(t, maxContextFileBytes, budget.bytes)
	})
	t.Run("per-file one-over", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "over.md")
		require.NoError(t, os.WriteFile(path, bytesOf('a', maxContextFileBytes+1), 0o600))
		budget := contextBudget{}
		require.Nil(t, readContextFile(context.Background(), path, &budget))
		require.Zero(t, budget.bytes)
	})
	t.Run("aggregate exact and one-over", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "aggregate.md")
		require.NoError(t, os.WriteFile(path, []byte("xy"), 0o600))
		budget := contextBudget{bytes: maxContextBytes - 2}
		require.NotNil(t, readContextFile(context.Background(), path, &budget))

		path = filepath.Join(t.TempDir(), "aggregate-over.md")
		require.NoError(t, os.WriteFile(path, []byte("xyz"), 0o600))
		oneOver := contextBudget{bytes: maxContextBytes - 2}
		require.Nil(t, readContextFile(context.Background(), path, &oneOver))
		require.Equal(t, maxContextBytes-2, oneOver.bytes)
	})
	t.Run("count one-over", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "count.md")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		opens := 0
		ctx := withContextFileOpener(context.Background(), func(path string) (contextFile, error) {
			opens++
			return os.Open(path)
		})
		budget := contextBudget{files: maxContextFiles - 1}
		require.NotNil(t, readContextFile(ctx, path, &budget))
		require.Nil(t, readContextFile(ctx, path, &budget))
		require.Equal(t, maxContextFiles, budget.files)
		require.Equal(t, 1, opens, "the one-over entry must be rejected before opening it")
	})
}

func TestBuild_ContextUsesOneAggregateBudgetAcrossProjectAndGlobal(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "context")
	require.NoError(t, os.Mkdir(project, 0o700))
	for i := 0; i < maxContextBytes/maxContextFileBytes; i++ {
		name := filepath.Join(project, fmt.Sprintf("%02d.md", i))
		require.NoError(t, os.WriteFile(name, bytesOf(byte('a'+i), maxContextFileBytes), 0o600))
	}
	global := filepath.Join(t.TempDir(), "global.md")
	require.NoError(t, os.WriteFile(global, []byte("global must be skipped"), 0o600))

	got := contextTestPrompt(t, root, []string{"context"}, []string{global})
	require.NotContains(t, got, "global must be skipped")
}

func TestBuild_ContextTraversalStopsAtBoundedEntryBudget(t *testing.T) {
	root := t.TempDir()
	dir := &generatedContextDirectory{total: maxContextEntries + 1}
	fileAttempts := 0
	ctx := withContextDirectoryOpener(context.Background(), func(string) (contextDirectory, error) {
		return dir, nil
	})
	ctx = withContextFileOpener(ctx, func(string) (contextFile, error) {
		fileAttempts++
		return nil, errors.New("generated unreadable entry")
	})
	ctx = withContextFileInspector(ctx, func(string) bool { return true })
	cfg := &config.Config{Options: &config.Options{GlobalContextPaths: []string{root}}}
	store := config.NewLibraryStore(cfg, root)
	p, err := NewPrompt("bounded-directory", "{{range .GlobalContextFiles}}{{.Content}}{{end}}")
	require.NoError(t, err)
	_, err = p.Build(ctx, "", "", store, cfg, false)
	require.NoError(t, err)
	require.Equal(t, maxContextEntries-1, dir.entriesReturned)
	require.Equal(t, maxContextEntries-1, fileAttempts)
	require.Less(t, dir.entriesReturned, dir.total,
		"the entry after the hard bound must not be read or opened")
}

func TestLoadContextFiles_DuplicateConsumesBudgetBeforeDedupe(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.md")
	second := filepath.Join(root, "second.md")
	content := []byte("duplicate instructions")
	require.NoError(t, os.WriteFile(first, content, 0o600))
	require.NoError(t, os.WriteFile(second, content, 0o600))
	budget := contextBudget{bytes: maxContextBytes - 2*len(content)}
	store := config.NewLibraryStore(&config.Config{}, root)
	files := loadContextFilesWithBudget(context.Background(), []string{first, second}, store, &budget)
	flattened := dedupeContextFiles(flattenContextFiles(files))
	require.Len(t, flattened, 1)
	require.Equal(t, maxContextBytes, budget.bytes)
}

func TestReadContextFile_CancellationClosesBlockedReader(t *testing.T) {
	blocked := newBlockedContextFile()

	ctx := withContextFileOpener(context.Background(), func(string) (contextFile, error) { return blocked, nil })
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan *ContextFile, 1)
	go func() { done <- readContextFile(ctx, "blocked", &contextBudget{}) }()
	doneReceived := false
	t.Cleanup(func() {
		cancel()
		_ = blocked.Close()
		if !doneReceived {
			awaitContextEvent(t, done, "reader completion during cleanup")
		}
		awaitContextEvent(t, blocked.readFinished, "reader goroutine completion during cleanup")
	})
	if _, ok := awaitContextEvent(t, blocked.readStarted, "reader start"); !ok {
		return
	}
	cancel()
	result, ok := awaitContextEvent(t, done, "reader completion")
	doneReceived = ok
	if !ok {
		return
	}
	if _, ok := awaitContextEvent(t, blocked.readFinished, "reader goroutine completion"); !ok {
		return
	}
	require.Nil(t, result)
	require.True(t, blocked.wasClosed())
}

func awaitContextEvent[T any](t *testing.T, event <-chan T, label string) (T, bool) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case value := <-event:
		return value, true
	case <-timer.C:
		t.Errorf("Timed out waiting for %s", label)
		var zero T
		return zero, false
	}
}

func TestReadContextFile_GeneratedReaderIsBounded(t *testing.T) {
	generated := &generatedContextFile{remaining: maxContextFileBytes + 1}
	ctx := withContextFileOpener(context.Background(), func(string) (contextFile, error) { return generated, nil })

	require.Nil(t, readContextFile(ctx, "generated", &contextBudget{}))
	require.Equal(t, maxContextFileBytes+1, generated.maxRequest)
}

func bytesOf(value byte, count int) []byte {
	return []byte(strings.Repeat(string(value), count))
}

type generatedContextFile struct {
	remaining  int
	maxRequest int
}

type generatedContextDirectory struct {
	total           int
	next            int
	entriesReturned int
}

func (d *generatedContextDirectory) ReadDir(n int) ([]os.DirEntry, error) {
	if d.next >= d.total {
		return nil, io.EOF
	}
	count := min(n, d.total-d.next)
	entries := make([]os.DirEntry, count)
	for i := range entries {
		entries[i] = generatedDirEntry{name: fmt.Sprintf("%08d.md", d.next+i)}
	}
	d.next += count
	d.entriesReturned += count
	return entries, nil
}

func (d *generatedContextDirectory) Close() error { return nil }

type generatedDirEntry struct{ name string }

func (e generatedDirEntry) Name() string               { return e.name }
func (e generatedDirEntry) IsDir() bool                { return false }
func (e generatedDirEntry) Type() os.FileMode          { return 0o600 }
func (e generatedDirEntry) Info() (os.FileInfo, error) { return generatedFileInfo{}, nil }

func (f *generatedContextFile) Read(p []byte) (int, error) {
	if len(p) > f.maxRequest {
		f.maxRequest = len(p)
	}
	if f.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), f.remaining)
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	f.remaining -= n
	return n, nil
}

func (f *generatedContextFile) Stat() (os.FileInfo, error) {
	return generatedFileInfo{}, nil
}

func (f *generatedContextFile) Close() error { return nil }

type generatedFileInfo struct{}

func (generatedFileInfo) Name() string       { return "generated" }
func (generatedFileInfo) Size() int64        { return 0 }
func (generatedFileInfo) Mode() os.FileMode  { return 0o600 }
func (generatedFileInfo) ModTime() time.Time { return time.Time{} }
func (generatedFileInfo) IsDir() bool        { return false }
func (generatedFileInfo) Sys() any           { return nil }

type blockedContextFile struct {
	readStarted  chan struct{}
	readFinished chan struct{}
	closed       chan struct{}
	closeOnce    sync.Once
	readOnce     sync.Once
}

func newBlockedContextFile() *blockedContextFile {
	return &blockedContextFile{
		readStarted:  make(chan struct{}),
		readFinished: make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (f *blockedContextFile) Read([]byte) (int, error) {
	defer f.readOnce.Do(func() { close(f.readFinished) })
	select {
	case <-f.readStarted:
	default:
		close(f.readStarted)
	}
	<-f.closed
	return 0, errors.New("closed")
}

func (f *blockedContextFile) Stat() (os.FileInfo, error) { return generatedFileInfo{}, nil }

func (f *blockedContextFile) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}

func (f *blockedContextFile) wasClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}
