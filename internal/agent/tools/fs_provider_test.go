package tools

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/stretchr/testify/require"
)

// flattenFileHits turns a fsGrepSearchContext-shaped collector map into
// the same []SearchLine shape OSDisk.Search produces, so a test can
// compare the two without caring about map iteration order (callers use
// require.ElementsMatch).
func flattenFileHits(files map[string]*fsGrepFileHits) []SearchLine {
	var lines []SearchLine
	for path, collector := range files {
		for lineNum, line := range collector.lines {
			lines = append(lines, SearchLine{Path: path, Line: lineNum, Text: line.text, Hit: line.hit})
		}
	}
	return lines
}

func TestOSDisk_StatMissingPathIsFsErrNotExist(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "does-not-exist.txt")

	_, err := OSDisk().Stat(t.Context(), missing)
	require.Error(t, err)
	require.True(t, errors.Is(err, fs.ErrNotExist))
	require.True(t, errors.Is(err, ErrNotExist))
}

func TestOSDisk_StatReportsDirRegularAndModTime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	file := filepath.Join(dir, "f.txt")
	require.NoError(t, os.WriteFile(file, []byte("hello"), 0o644))

	dirInfo, err := OSDisk().Stat(t.Context(), sub)
	require.NoError(t, err)
	require.True(t, dirInfo.IsDir())

	fileInfo, err := OSDisk().Stat(t.Context(), file)
	require.NoError(t, err)
	require.False(t, fileInfo.IsDir())
	require.True(t, fileInfo.Mode().IsRegular())

	wantInfo, err := os.Stat(file)
	require.NoError(t, err)
	require.Equal(t, wantInfo.ModTime(), fileInfo.ModTime())
}

func TestOSDisk_EvalSymlinksResolvesThroughLink(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	target := filepath.Join(tmp, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))
	link := filepath.Join(tmp, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	got, err := OSDisk().EvalSymlinks(t.Context(), link)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(link)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.NotEqual(t, link, got, "must resolve THROUGH the symlink, not just clean it")

	content, err := os.ReadFile(got)
	require.NoError(t, err)
	require.Equal(t, "x", string(content))
}

func TestOSDisk_OpenStreamsWithoutReadingWholeFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")

	var b strings.Builder
	const first = "first line\n"
	b.WriteString(first)
	filler := strings.Repeat("x", 1024) + "\n"
	for range 5 * 1024 {
		b.WriteString(filler)
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))

	rc, err := OSDisk().Open(t.Context(), path)
	require.NoError(t, err)
	reader := bufio.NewReader(rc)
	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, first, line)
	require.NoError(t, rc.Close())
}

func TestOSDisk_WriteFileIsAtomicAndLeavesNoTempFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	data := []byte("hello atomic world")

	require.NoError(t, OSDisk().WriteFile(t.Context(), path, data, 0o644))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no *.tmp residue must remain next to the target")
	require.Equal(t, "out.txt", entries[0].Name())

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, data, got)

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	}
}

func TestOSDisk_WriteFileFailureLeavesOriginalIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory attribute does not reliably block file creation on Windows")
	}
	t.Parallel()
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(sub, 0o755))
	path := filepath.Join(sub, "f.txt")
	original := []byte("original content")
	require.NoError(t, os.WriteFile(path, original, 0o644))

	require.NoError(t, os.Chmod(sub, 0o500))
	defer func() { _ = os.Chmod(sub, 0o755) }()

	err := OSDisk().WriteFile(t.Context(), path, []byte("new content"), 0o644)
	require.Error(t, err)

	require.NoError(t, os.Chmod(sub, 0o755))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, original, got)
}

func TestOSDisk_MkdirAllThenRemove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "c")

	require.NoError(t, OSDisk().MkdirAll(t.Context(), nested, 0o755))
	info, err := os.Stat(nested)
	require.NoError(t, err)
	require.True(t, info.IsDir())

	file := filepath.Join(nested, "f.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))

	require.NoError(t, OSDisk().Remove(t.Context(), file))
	_, err = os.Stat(file)
	require.True(t, errors.Is(err, fs.ErrNotExist))
}

func TestOSDisk_ListMatchesFsextListDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "keep.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "skip.log"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "top.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644))

	req := ListRequest{Dir: dir}
	got, err := OSDisk().List(t.Context(), req)
	require.NoError(t, err)

	wantEntries, wantTruncated, err := fsext.ListDirectory(dir, nil, 0, 0)
	require.NoError(t, err)

	// fastwalk parallelises the walk across directories, so two separate
	// calls (this one and the one inside OSDisk.List) can return the same
	// SET of entries in a different order — order-independent comparison
	// is the correct "byte-for-byte" proof, not a literal slice Equal.
	require.ElementsMatch(t, wantEntries, got.Entries)
	require.Equal(t, wantTruncated, got.Truncated)

	for _, e := range got.Entries {
		require.NotContains(t, e, "skip.log", "gitignored file must not appear in either listing")
	}
}

func TestOSDisk_ListEntriesKeepTrailingSeparatorOnDirectories(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "subdir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0o644))

	got, err := OSDisk().List(t.Context(), ListRequest{Dir: dir})
	require.NoError(t, err)

	var dirEntry, fileEntry string
	for _, e := range got.Entries {
		switch filepath.Base(strings.TrimSuffix(e, string(filepath.Separator))) {
		case "subdir":
			dirEntry = e
		case "file.txt":
			fileEntry = e
		}
	}
	require.NotEmpty(t, dirEntry)
	require.NotEmpty(t, fileEntry)
	require.True(t, strings.HasSuffix(dirEntry, string(filepath.Separator)),
		"directory entry must end with a native separator: %q", dirEntry)
	require.False(t, strings.HasSuffix(fileEntry, string(filepath.Separator)),
		"file entry must not end with a separator: %q", fileEntry)
	// Entries may arrive forward-slashed (fastwalk's ToSlash) with only the
	// directory's trailing separator kept native — see fs_list.go's own
	// comment on this exact quirk. FromSlash is what every real consumer
	// (fs_list.go:138) runs before matching against the native Dir, so the
	// "starts with Dir verbatim" contract is checked post-FromSlash, which
	// is a no-op on platforms where the two separators coincide.
	require.True(t, strings.HasPrefix(filepath.FromSlash(dirEntry), dir),
		"entry must start with the request's Dir once native-separated: %q", dirEntry)
	require.True(t, strings.HasPrefix(filepath.FromSlash(fileEntry), dir),
		"entry must start with the request's Dir once native-separated: %q", fileEntry)
}

func TestOSDisk_FindMatchesGlobFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "c.txt"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "d.md"), []byte("x"), 0o644))

	got, err := OSDisk().Find(t.Context(), FindRequest{Pattern: "**/*.txt", Dir: dir, Limit: 100})
	require.NoError(t, err)

	wantPaths, wantTruncated, err := globFiles(t.Context(), "**/*.txt", dir, 100)
	require.NoError(t, err)

	// The fallback engine (always used under go test) sorts by ModTime,
	// with ties broken by fastwalk's own goroutine completion order, which
	// is not guaranteed stable across two separate calls — compare sets,
	// not sequences.
	require.ElementsMatch(t, wantPaths, got.Paths)
	require.Equal(t, wantTruncated, got.Truncated)
	require.NotEmpty(t, got.Paths)
}

func TestOSDisk_SearchReproducesFallbackWindows(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Same fixture shape as fs_grep_test.go's fallback-window test: hits
	// at 5, 8 and 20 with radius 2, so two windows merge and the last is
	// clamped to the file's final line.
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	lines[4] = "line 05 needle"
	lines[7] = "line 08 needle"
	lines[19] = "line 20 needle"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "code.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	wantFiles := map[string]*fsGrepFileHits{}
	wantBudget := newFSGrepBudget()
	require.NoError(t, fsGrepSearchContext(t.Context(), "needle", dir, "", 2, wantFiles, &wantBudget))
	want := flattenFileHits(wantFiles)
	require.NotEmpty(t, want)

	got, err := OSDisk().Search(t.Context(), SearchRequest{Pattern: "needle", Dir: dir, ContextLines: 2})
	require.NoError(t, err)

	require.ElementsMatch(t, want, got.Lines)
}

func TestOSDisk_SearchWithRealRipgrepAgreesWithFallback(t *testing.T) {
	t.Parallel()
	rgPath, err := exec.LookPath("rg")
	if err != nil {
		t.Skip("rg is not in $PATH")
	}
	dir := t.TempDir()

	lines := make([]string, 12)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i+1)
	}
	lines[4] = "line 05 needle"
	lines[7] = "line 08 needle"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "code.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	// Run the real rg binary directly (bypassing the testing.Testing()
	// guard in getRg), mirroring fs_grep_test.go's own precedent.
	cmd := buildRgSearchCmd(t.Context(), rgPath, "needle", dir, "", 2)
	rgFiles := map[string]*fsGrepFileHits{}
	rgBudget := newFSGrepBudget()
	require.NoError(t, runRipgrepContextSearch(t.Context(), cmd, rgFiles, &rgBudget))
	want := flattenFileHits(rgFiles)
	require.NotEmpty(t, want)

	// OSDisk.Search runs under go test, where getRg() always returns ""
	// (testing.Testing() guard), so this exercises the fallback engine.
	got, err := OSDisk().Search(t.Context(), SearchRequest{Pattern: "needle", Dir: dir, ContextLines: 2})
	require.NoError(t, err)

	require.ElementsMatch(t, want, got.Lines)
}

func TestOSDisk_SearchHonoursMaxLinesHint(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	lines := make([]string, 50)
	for i := range lines {
		lines[i] = "needle"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Join(lines, "\n")+"\n"), 0o644))

	got, err := OSDisk().Search(t.Context(), SearchRequest{Pattern: "needle", Dir: dir, ContextLines: 0, MaxLines: 5})
	require.NoError(t, err)
	require.Len(t, got.Lines, 5)
}

func TestDiskOrOS_NilIsTheRealDisk(t *testing.T) {
	t.Parallel()
	require.Equal(t, OSDisk(), diskOrOS(nil))
}
