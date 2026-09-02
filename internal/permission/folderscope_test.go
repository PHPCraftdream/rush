package permission

import (
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allOps enumerates every known operation, for deny-everything
// assertions.
var allOps = []FileOp{
	FileOpList, FileOpFind, FileOpGrep, FileOpRead, FileOpCreate,
	FileOpOverwrite, FileOpWriteLines, FileOpReplace, FileOpDelete,
}

// requireScopeDenied asserts that err is a typed scope denial for the
// given path and op, and returns it for further assertions.
func requireScopeDenied(t *testing.T, err error, path string, op FileOp) *ScopeDeniedError {
	t.Helper()
	require.Error(t, err, "expected denial of %s on %q", op, path)
	var denied *ScopeDeniedError
	require.ErrorAs(t, err, &denied, "denials are typed *ScopeDeniedError")
	assert.Equal(t, path, denied.Path, "denial echoes the path as checked")
	assert.Equal(t, op, denied.Op)
	assert.NotEmpty(t, denied.Reason)
	return denied
}

func TestFolderScope_LongestMatchWins(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sub := filepath.Join(root, "src")
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{
			{Dir: root, Ops: []FileOp{FileOpRead, FileOpGrep}},
			{Dir: sub, Ops: []FileOp{FileOpRead, FileOpWriteLines}},
		},
	})
	require.NoError(t, err)

	// The deeper entry wins over the enclosing one.
	require.NoError(t, scope.Check(filepath.Join(sub, "a.go"), FileOpWriteLines))
	require.NoError(t, scope.Check(filepath.Join(sub, "a.go"), FileOpRead))
	// Matching ONE entry is not a union of overlapping entries: the
	// parent grants grep, the matched child does not, so grep is denied
	// under the child even though an enclosing entry grants it.
	denied := requireScopeDenied(t,
		scope.Check(filepath.Join(sub, "a.go"), FileOpGrep),
		filepath.Join(sub, "a.go"), FileOpGrep)
	assert.Contains(t, denied.Reason, "not granted by scope")
	// The parent still grants its own ops outside the child.
	require.NoError(t, scope.Check(filepath.Join(root, "README.md"), FileOpGrep))
	// The scope dir itself matches its own entry (Rel yields ".").
	require.NoError(t, scope.Check(sub, FileOpRead))
}

func TestFolderScope_CarveOutExcludesSubtree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{
			{Dir: root, Ops: []FileOp{FileOpRead, FileOpList, FileOpGrep}},
			{Dir: secrets}, // Ops empty: deny carve-out
		},
	})
	require.NoError(t, err)

	for _, op := range []FileOp{FileOpRead, FileOpList, FileOpGrep} {
		denied := requireScopeDenied(t,
			scope.Check(filepath.Join(secrets, "keys.txt"), op),
			filepath.Join(secrets, "keys.txt"), op)
		assert.Contains(t, denied.Reason, "deny-carved scope",
			"carve-out denials name the rule, not just op membership")
	}
	// The carve-out does not affect siblings.
	require.NoError(t, scope.Check(filepath.Join(root, "open.txt"), FileOpRead))
	// The coarse decision still reports the parent's grants, but the
	// carved subtree is not offered as a root for them.
	assert.True(t, scope.Grants(FileOpRead))
	assert.Equal(t, []string{root}, scope.Roots(FileOpRead))
}

func TestFolderScope_EscapingPathDenied(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{{Dir: src, Ops: []FileOp{FileOpRead}}},
	})
	require.NoError(t, err)

	// A sibling of the scope relates as "../<name>" — denied.
	sibling := filepath.Join(root, "outside.txt")
	denied := requireScopeDenied(t, scope.Check(sibling, FileOpRead), sibling, FileOpRead)
	assert.Contains(t, denied.Reason, "outside every folder scope")

	// Traversal spelling cleans to a path outside the scope, so it is
	// denied too.
	traversal := filepath.Join(src, "..", "outside.txt")
	require.Equal(t, sibling, filepath.Clean(traversal),
		"test premise: the traversal spelling denotes the sibling")
	requireScopeDenied(t, scope.Check(traversal, FileOpRead), traversal, FileOpRead)

	// A relative path can never be contained by an absolute entry dir,
	// so contract violations fail closed.
	relative := filepath.Join("src", "a.go")
	requireScopeDenied(t, scope.Check(relative, FileOpRead), relative, FileOpRead)

	// The empty path never matches anything.
	requireScopeDenied(t, scope.Check("", FileOpRead), "", FileOpRead)
}

func TestFolderScope_CrossVolumeDeniedOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("cross-volume Rel errors are a Windows drive-letter behaviour")
	}
	t.Parallel()
	// Pure string logic: neither path needs to exist; Rel refuses to
	// relate different volume names, which must deny.
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{{Dir: `C:\scoped`, Ops: []FileOp{FileOpRead}}},
	})
	require.NoError(t, err)

	denied := requireScopeDenied(t,
		scope.Check(`D:\elsewhere\file.txt`, FileOpRead),
		`D:\elsewhere\file.txt`, FileOpRead)
	assert.Contains(t, denied.Reason, "outside every folder scope")
}

func TestFolderScope_WindowsCaseInsensitive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("filepath.Rel compares case-insensitively only on Windows")
	}
	t.Parallel()
	root := t.TempDir()
	src := filepath.Join(root, "src")

	// An entry dir stored all-lowercase still contains real-case paths.
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{
			{Dir: strings.ToLower(src), Ops: []FileOp{FileOpRead}},
		},
	})
	require.NoError(t, err)
	require.NoError(t, scope.Check(filepath.Join(src, "a.go"), FileOpRead))

	// And a real-case entry contains an all-uppercase path spelling.
	scopeUpper, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{{Dir: src, Ops: []FileOp{FileOpRead}}},
	})
	require.NoError(t, err)
	require.NoError(t, scopeUpper.Check(strings.ToUpper(filepath.Join(src, "a.go")), FileOpRead))
}

func TestFolderScope_ZeroValueDeniesEverything(t *testing.T) {
	t.Parallel()
	// The load-bearing fail-closed invariant: a zero value — a
	// nil-derived or forgotten scope — must never mean "unrestricted".
	var scope FolderScope
	abs := filepath.Join(t.TempDir(), "file.txt")
	for _, op := range allOps {
		requireScopeDenied(t, scope.Check(abs, op), abs, op)
		assert.False(t, scope.Grants(op))
		assert.Empty(t, scope.Roots(op))
	}
	assert.False(t, scope.KeepsCommandTools())
	// Denials are still typed, with the no-entry reason.
	denied := requireScopeDenied(t, scope.Check(abs, FileOpRead), abs, FileOpRead)
	assert.Contains(t, denied.Reason, "outside every folder scope")
}

func TestBuildFolderScope_HardErrorOnMalformedEntry(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// What counts as malformed, and why each variant must fail the
	// WHOLE spec rather than be dropped (the zero-value scope denies
	// everything, so a failed build can never leak partial grants):
	//
	//   - empty Dir: no directory is denoted. BuildRunAllowlist drops
	//     bad entries because dropping a bad GRANT narrows; a
	//     folder-scope entry could equally be a carve-out, and dropping
	//     a bad carve-out would WIDEN access — the compiler cannot tell
	//     which the host meant.
	//   - relative Dir with empty WorkingDir: unresolvable without
	//     silently inventing the process working directory.
	//   - unknown operation: a typo in a JSON- or flag-authored spec
	//     would otherwise become a silently dead grant.

	t.Run("empty dir", func(t *testing.T) {
		t.Parallel()
		_, err := BuildFolderScope(FolderScopeSpec{
			Entries: []FolderScopeEntry{
				{Dir: root, Ops: []FileOp{FileOpRead}},
				{Dir: "   "},
			},
		})
		require.Error(t, err)
		assert.ErrorContains(t, err, "Dir is empty")
	})

	t.Run("whole spec refused and result denies everything", func(t *testing.T) {
		t.Parallel()
		scope, err := BuildFolderScope(FolderScopeSpec{
			Entries: []FolderScopeEntry{
				{Dir: root, Ops: []FileOp{FileOpRead}}, // valid, must not survive either
				{Dir: ""},
			},
		})
		require.Error(t, err)
		inside := filepath.Join(root, "a.txt")
		for _, op := range allOps {
			assert.False(t, scope.Grants(op), "no partial grants may leak from a failed build")
			assert.Empty(t, scope.Roots(op))
			requireScopeDenied(t, scope.Check(inside, op), inside, op)
		}
	})

	t.Run("relative dir without working dir", func(t *testing.T) {
		t.Parallel()
		_, err := BuildFolderScope(FolderScopeSpec{
			Entries: []FolderScopeEntry{{Dir: "src", Ops: []FileOp{FileOpRead}}},
		})
		require.Error(t, err)
		assert.ErrorContains(t, err, "WorkingDir is empty")
	})

	t.Run("unknown operation", func(t *testing.T) {
		t.Parallel()
		_, err := BuildFolderScope(FolderScopeSpec{
			Entries: []FolderScopeEntry{
				{Dir: filepath.Join(root, "src"), Ops: []FileOp{"writ"}},
			},
		})
		require.Error(t, err)
		assert.ErrorContains(t, err, "unknown operation")
	})
}

func TestBuildFolderScope_ResolvesRelativeDirsAgainstWorkingDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scope, err := BuildFolderScope(FolderScopeSpec{
		WorkingDir: root,
		Entries: []FolderScopeEntry{
			{Dir: "src", Ops: []FileOp{FileOpRead}},
			{Dir: ".", Ops: []FileOp{FileOpList}},
		},
	})
	require.NoError(t, err)

	require.NoError(t, scope.Check(filepath.Join(root, "src", "a.go"), FileOpRead))
	// "." compiles to the working dir itself.
	require.NoError(t, scope.Check(root, FileOpList))
	// Longest match still applies to resolved entries: under src the
	// read-only entry wins, so list is denied there but works beside it.
	requireScopeDenied(t,
		scope.Check(filepath.Join(root, "src", "a.go"), FileOpList),
		filepath.Join(root, "src", "a.go"), FileOpList)
	require.NoError(t, scope.Check(filepath.Join(root, "b.txt"), FileOpList))
}

func TestFolderScope_GrantsAndRootsAcrossOpSets(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	docs := filepath.Join(root, "docs")
	build := filepath.Join(root, "build")
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{
			{Dir: docs, Ops: []FileOp{FileOpRead, FileOpGrep}},
			{Dir: build, Ops: []FileOp{FileOpWriteLines, FileOpDelete}},
			{Dir: filepath.Join(root, "secrets")}, // carve-out grants nothing
		},
		KeepCommandTools: true,
	})
	require.NoError(t, err)

	assert.True(t, scope.Grants(FileOpRead))
	assert.True(t, scope.Grants(FileOpGrep))
	assert.True(t, scope.Grants(FileOpWriteLines))
	assert.True(t, scope.Grants(FileOpDelete))
	assert.False(t, scope.Grants(FileOpList))
	assert.False(t, scope.Grants(FileOp("not-an-op")))
	assert.True(t, scope.KeepsCommandTools())

	assert.Equal(t, []string{docs}, scope.Roots(FileOpRead))
	assert.Equal(t, []string{build}, scope.Roots(FileOpDelete))
	assert.Empty(t, scope.Roots(FileOpList))
}

func TestFolderScope_RootsDeepestFirst(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{
			{Dir: root, Ops: []FileOp{FileOpRead}},
			{Dir: sub, Ops: []FileOp{FileOpRead, FileOpGrep}},
		},
	})
	require.NoError(t, err)

	// Roots follow matcher order: deepest first.
	assert.Equal(t, []string{sub, root}, scope.Roots(FileOpRead))
	assert.Equal(t, []string{sub}, scope.Roots(FileOpGrep))
}

func TestFolderScope_DuplicateDirsFirstEntryWins(t *testing.T) {
	t.Parallel()
	// Duplicate dirs must not union: the first spec entry wins, which
	// keeps matching deterministic when a host repeats a dir.
	root := t.TempDir()
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{
			{Dir: root, Ops: []FileOp{FileOpRead}},
			{Dir: root, Ops: []FileOp{FileOpRead, FileOpWriteLines}},
		},
	})
	require.NoError(t, err)

	require.NoError(t, scope.Check(filepath.Join(root, "a.txt"), FileOpRead))
	requireScopeDenied(t,
		scope.Check(filepath.Join(root, "a.txt"), FileOpWriteLines),
		filepath.Join(root, "a.txt"), FileOpWriteLines)
}

func TestFolderScope_ConcurrentUse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scope, err := BuildFolderScope(FolderScopeSpec{
		Entries: []FolderScopeEntry{
			{Dir: root, Ops: append([]FileOp{}, allOps...)},
			{Dir: filepath.Join(root, "secrets")},
		},
	})
	require.NoError(t, err)

	inside := filepath.Join(root, "a.txt")
	outside := filepath.Join(root, "secrets", "b.txt")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				assert.NoError(t, scope.Check(inside, FileOpRead))
				assert.Error(t, scope.Check(outside, FileOpRead))
				assert.True(t, scope.Grants(FileOpRead))
				assert.Equal(t, []string{root}, scope.Roots(FileOpList))
				assert.False(t, scope.KeepsCommandTools())
			}
		}()
	}
	wg.Wait()
}

func TestScopeDeniedError_ErrorAndAs(t *testing.T) {
	t.Parallel()
	err := error(&ScopeDeniedError{
		Path:   `C:\x\a.txt`,
		Op:     FileOpRead,
		Reason: "outside every folder scope",
	})
	// %q renders the path as a Go-quoted literal, so each Windows
	// separator is doubled in the message.
	assert.Equal(t,
		`folderscope: denied read of "C:\\x\\a.txt" — outside every folder scope`,
		err.Error())

	var denied *ScopeDeniedError
	require.True(t, errors.As(err, &denied))
	assert.Equal(t, FileOpRead, denied.Op)
}
