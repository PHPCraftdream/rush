package tools

// R5-2 regressions (P0 security review,
// docs/reviews/2026-09-02-sdk-library-review-round-5-2331.md): production
// folder-scope compilation used to only filepath.Join+Clean a relative
// entry onto WorkingDir -- never resolving symlinks -- while every
// REQUESTED item path went through resolveScopedPath (longest-existing-
// prefix + EvalSymlinks) before permission.FolderScope.Check ever saw it.
// A scope's compiled roots and a request's resolved item path could
// therefore land in two different namespaces, letting a symlinked deny
// carve-out silently stop matching while its broader parent grant kept
// matching. CanonicalizeFolderScopeSpec closes the gap by running entries
// (and WorkingDir itself) through the exact same algorithm and the exact
// same DiskProvider that resolves item paths.
//
// Four regressions are required: a REAL symlinked deny carve-out nested
// under a broader grant, a REAL symlinked WorkingDir, and both cases again
// through a FAKE DiskProvider whose EvalSymlinks resolves a path
// differently than its lexical spelling -- proving canonicalization
// actually consults the active provider instead of hardcoding the real
// filepath.EvalSymlinks.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// TestCanonicalizeFolderScopeSpec_RealSymlinkedDenyCarveOutUnderBroaderGrant
// reproduces the review's concrete bypass with an ACTUAL os.Symlink:
// "/work" grants read, "/work/alias" is a deny carve-out, and
// "/work/alias" is really a symlink to "/work/private". Before
// canonicalization, the carve-out's compiled root ("/work/alias", never
// resolved) does not lexically contain the resolved item path
// ("/work/private/key.txt"), so only the broader "/work" grant matches and
// the carve-out is silently bypassed. After canonicalization both sides
// compare in the same, symlink-resolved namespace and the carve-out denies
// correctly.
func TestCanonicalizeFolderScopeSpec_RealSymlinkedDenyCarveOutUnderBroaderGrant(t *testing.T) {
	t.Parallel()
	// t.TempDir() itself is not guaranteed to already be in canonical
	// form -- some CI runner images mount/junction the OS temp root
	// (macOS's /var -> /private/var; Windows runners with a redirected
	// temp drive), the same host-topology class of issue already fixed
	// in 73878311 and mirrored by this file's own
	// TestCanonicalizeFolderScopeSpec_RealSymlinkedWorkingDir (which
	// resolves its own "real" dir before comparing). Without this, the
	// premise assertion below compares a fully symlink-resolved
	// resolvedItem against an unresolved "work" grant root and can
	// spuriously deny on such a runner even though the entry-level bug
	// this test targets (the unresolved "alias" carve-out) is unrelated
	// to WorkingDir's own canonical form.
	work, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	private := filepath.Join(work, "private")
	require.NoError(t, os.MkdirAll(private, 0o755))
	keyFile := filepath.Join(private, "key.txt")
	require.NoError(t, os.WriteFile(keyFile, []byte("secret"), 0o644))

	alias := filepath.Join(work, "alias")
	if err := os.Symlink(private, alias); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	spec := permission.FolderScopeSpec{
		WorkingDir: work,
		Entries: []permission.FolderScopeEntry{
			{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead}},
			{Dir: "alias"}, // empty Ops: deny carve-out
		},
	}

	ctx := context.Background()
	resolvedItem, err := resolveScopedPath(ctx, OSDisk(), work, filepath.Join("alias", "key.txt"))
	require.NoError(t, err)

	// Pre-fix premise: compiling the RAW spec directly (no
	// canonicalization) leaves the carve-out root lexically spelled
	// "alias", which does not contain the resolved item path at all --
	// the item falls through to the broader grant instead of being
	// denied. This documents the exact vulnerability being closed.
	uncanonScope, err := permission.BuildFolderScope(spec)
	require.NoError(t, err)
	require.NoError(t, uncanonScope.Check(resolvedItem, permission.FileOpRead),
		"test premise: the UNCANONICALIZED scope must widen through the carve-out, proving the bypass exists without the fix")

	// Fixed path: canonicalize before compiling.
	canon, err := CanonicalizeFolderScopeSpec(ctx, nil, spec)
	require.NoError(t, err)
	scope, err := permission.BuildFolderScope(canon)
	require.NoError(t, err)

	err = scope.Check(resolvedItem, permission.FileOpRead)
	require.Error(t, err, "a symlinked deny carve-out must still deny the resolved item path")
	var denied *permission.ScopeDeniedError
	require.ErrorAs(t, err, &denied)
	require.Contains(t, denied.Reason, "deny-carved scope")

	// The parent grant still works for a sibling outside the carve-out.
	sibling := filepath.Join(work, "open.txt")
	require.NoError(t, os.WriteFile(sibling, []byte("x"), 0o644))
	require.NoError(t, scope.Check(sibling, permission.FileOpRead))
}

// TestCanonicalizeFolderScopeSpec_RealSymlinkedWorkingDir covers the
// second required regression: WorkingDir ITSELF is a real symlink. Before
// canonicalization, a "." entry compiles to the symlinked (unresolved)
// WorkingDir spelling, while a requested item path resolves through the
// REAL target -- a namespace mismatch identical in kind to the carve-out
// case, just against the working directory instead of a nested entry.
func TestCanonicalizeFolderScopeSpec_RealSymlinkedWorkingDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	require.NoError(t, os.MkdirAll(real, 0o755))
	dataFile := filepath.Join(real, "data.txt")
	require.NoError(t, os.WriteFile(dataFile, []byte("hello"), 0o644))

	wdLink := filepath.Join(tmp, "wd-link")
	if err := os.Symlink(real, wdLink); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	spec := permission.FolderScopeSpec{
		WorkingDir: wdLink,
		Entries:    []permission.FolderScopeEntry{{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead}}},
	}

	ctx := context.Background()
	wantReal, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)

	resolvedItem, err := resolveScopedPath(ctx, OSDisk(), wdLink, "data.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(wantReal, "data.txt"), resolvedItem)

	// Pre-fix premise: the raw spec's "." entry compiles to the
	// UNRESOLVED symlinked WorkingDir, which does not lexically contain
	// the resolved item path at all -- a working scope becomes a
	// false-deny, the mirror-image namespace-mismatch bug.
	uncanonScope, err := permission.BuildFolderScope(spec)
	require.NoError(t, err)
	require.Error(t, uncanonScope.Check(resolvedItem, permission.FileOpRead),
		"test premise: the UNCANONICALIZED scope must fail to relate a symlinked WorkingDir to the resolved item path")

	canon, err := CanonicalizeFolderScopeSpec(ctx, nil, spec)
	require.NoError(t, err)
	require.Equal(t, wantReal, canon.WorkingDir)
	require.Len(t, canon.Entries, 1)
	require.Equal(t, wantReal, canon.Entries[0].Dir)

	scope, err := permission.BuildFolderScope(canon)
	require.NoError(t, err)
	require.NoError(t, scope.Check(resolvedItem, permission.FileOpRead))
}

// TestCanonicalizeFolderScopeSpec_FakeProviderSymlinkedDenyCarveOutUnderBroaderGrant
// is the fake-provider equivalent of the real-symlink carve-out test: the
// fake's EvalSymlinks resolves "alias" to a completely different path than
// its lexical spelling, something the real filepath.EvalSymlinks could
// never do for a path that isn't an actual symlink on disk. A pass here
// proves CanonicalizeFolderScopeSpec actually calls disk.EvalSymlinks on
// the ACTIVE provider, not a hardcoded real-disk resolution.
func TestCanonicalizeFolderScopeSpec_FakeProviderSymlinkedDenyCarveOutUnderBroaderGrant(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	alias := filepath.Join(workingDir, "alias")
	private := filepath.Join(workingDir, "private")
	disk.putDir(alias)
	disk.putDir(private)
	keyFile := filepath.Join(private, "key.txt")
	disk.putFile(keyFile, "secret")
	disk.mu.Lock()
	disk.symlinks = map[string]string{filepath.Clean(alias): private}
	disk.mu.Unlock()

	spec := permission.FolderScopeSpec{
		WorkingDir: workingDir,
		Entries: []permission.FolderScopeEntry{
			{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead}},
			{Dir: "alias"}, // empty Ops: deny carve-out
		},
	}

	ctx := context.Background()
	resolvedItem, err := resolveScopedPath(ctx, disk, workingDir, filepath.Join("alias", "key.txt"))
	require.NoError(t, err)
	require.Equal(t, keyFile, resolvedItem,
		"test premise: the fake provider resolves alias/key.txt to the private dir")

	canon, err := CanonicalizeFolderScopeSpec(ctx, disk, spec)
	require.NoError(t, err)
	scope, err := permission.BuildFolderScope(canon)
	require.NoError(t, err)

	err = scope.Check(resolvedItem, permission.FileOpRead)
	require.Error(t, err)
	var denied *permission.ScopeDeniedError
	require.ErrorAs(t, err, &denied)
	require.Contains(t, denied.Reason, "deny-carved scope")

	require.Contains(t, disk.Calls(), "EvalSymlinks:"+alias,
		"canonicalization must consult the injected provider's EvalSymlinks, not the real filesystem")
	requireRealDirEmpty(t, tmp)
}

// TestCanonicalizeFolderScopeSpec_FakeProviderSymlinkedWorkingDir is the
// fake-provider equivalent of the symlinked-WorkingDir test.
func TestCanonicalizeFolderScopeSpec_FakeProviderSymlinkedWorkingDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	base := filepath.Join(tmp, "virtual")
	wdLink := filepath.Join(base, "wd-link")
	real := filepath.Join(base, "real-target")
	disk := newFakeDisk()
	disk.putDir(base)
	disk.putDir(wdLink) // Stat follows the "symlink" like a real directory would
	disk.putDir(real)
	disk.putFile(filepath.Join(real, "data.txt"), "hello")
	disk.mu.Lock()
	disk.symlinks = map[string]string{filepath.Clean(wdLink): real}
	disk.mu.Unlock()

	spec := permission.FolderScopeSpec{
		WorkingDir: wdLink,
		Entries:    []permission.FolderScopeEntry{{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead}}},
	}

	ctx := context.Background()
	resolvedItem, err := resolveScopedPath(ctx, disk, wdLink, "data.txt")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(real, "data.txt"), resolvedItem)

	canon, err := CanonicalizeFolderScopeSpec(ctx, disk, spec)
	require.NoError(t, err)
	require.Equal(t, real, canon.WorkingDir)
	require.Len(t, canon.Entries, 1)
	require.Equal(t, real, canon.Entries[0].Dir)

	scope, err := permission.BuildFolderScope(canon)
	require.NoError(t, err)
	require.NoError(t, scope.Check(resolvedItem, permission.FileOpRead))

	require.Contains(t, disk.Calls(), "EvalSymlinks:"+wdLink,
		"canonicalization must consult the injected provider's EvalSymlinks for WorkingDir itself, not the real filesystem")
	requireRealDirEmpty(t, tmp)
}

// TestCanonicalizeFolderScopeSpec_MalformedEntriesPassThroughUnchanged pins
// that CanonicalizeFolderScopeSpec does not duplicate or interfere with
// BuildFolderScope's own malformed-entry validation: an empty Dir and a
// relative Dir with an empty WorkingDir are passed through untouched, so
// BuildFolderScope still produces its existing, well-tested error text.
func TestCanonicalizeFolderScopeSpec_MalformedEntriesPassThroughUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("empty dir", func(t *testing.T) {
		t.Parallel()
		spec := permission.FolderScopeSpec{
			WorkingDir: t.TempDir(),
			Entries:    []permission.FolderScopeEntry{{Dir: "   "}},
		}
		canon, err := CanonicalizeFolderScopeSpec(ctx, nil, spec)
		require.NoError(t, err)
		_, buildErr := permission.BuildFolderScope(canon)
		require.Error(t, buildErr)
		require.Contains(t, buildErr.Error(), "Dir is empty")
	})

	t.Run("relative dir without working dir", func(t *testing.T) {
		t.Parallel()
		spec := permission.FolderScopeSpec{
			Entries: []permission.FolderScopeEntry{{Dir: "src", Ops: []permission.FileOp{permission.FileOpRead}}},
		}
		canon, err := CanonicalizeFolderScopeSpec(ctx, nil, spec)
		require.NoError(t, err)
		require.Equal(t, "src", canon.Entries[0].Dir, "an unresolvable relative entry must pass through untouched")
		_, buildErr := permission.BuildFolderScope(canon)
		require.Error(t, buildErr)
		require.Contains(t, buildErr.Error(), "WorkingDir is empty")
	})
}

// TestCanonicalizeFolderScopeSpec_ResolutionErrorNeverPartial pins the
// fail-closed contract: when one entry cannot be resolved (here, a path
// component that is a file, not a directory), the function returns the
// zero FolderScopeSpec and an error -- never a spec with SOME entries
// resolved and others not, which a caller could mistake for a valid
// partial result.
func TestCanonicalizeFolderScopeSpec_ResolutionErrorNeverPartial(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "afile.txt"), []byte("x"), 0o644))

	spec := permission.FolderScopeSpec{
		WorkingDir: tmp,
		Entries: []permission.FolderScopeEntry{
			{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead}},
			// A file used as a directory component: resolveScopedPath
			// must refuse this.
			{Dir: filepath.Join("afile.txt", "sub"), Ops: []permission.FileOp{permission.FileOpRead}},
		},
	}
	canon, err := CanonicalizeFolderScopeSpec(context.Background(), nil, spec)
	require.Error(t, err)
	require.Equal(t, permission.FolderScopeSpec{}, canon,
		"a resolution error must return the zero spec, never a partially-resolved one")
}
