package tools

// R6-2 regressions (P1 security review,
// docs/reviews/2026-09-03-sdk-library-review-round-6-0808.md): R5-2
// canonicalized folder-scope ROOTS and every REQUESTED item path through
// the same resolveScopedPath algorithm, closing the direct-item symlink
// bypass. It never touched paths coming BACK from a traversal provider
// (fs_list's disk.List, fs_find's disk.Find): fsListOne/fsFindOne checked
// the provider's lexical/alias-spelled result directly against the
// canonicalized scope, a namespace mismatch identical in kind to the one
// R5-2 fixed, just on the result side instead of the request side.
//
// Concrete disclosure reproduced below: a broad grant over a parent
// directory, a nested deny carve-out over a REAL subdirectory, and a
// SIBLING symlink that aliases the denied subdirectory under a different
// name. A traversal that follows directory symlinks (OSDisk's fastwalk,
// Follow: true) returns the secret file under BOTH the real, denied path
// AND the alias path; only re-resolving each result through
// resolveScopedPath before Check (mirroring fs_grep's existing pattern)
// catches the alias spelling too.
//
// Three regressions, as required by the review:
//  1. a real OSDisk end-to-end test with an actual os.Symlink;
//  2. a fake-provider test for fs_list with an injected alias result;
//  3. a fake-provider test for fs_find with an injected alias result.
//
// All three assert the denied file NEVER appears anywhere in the
// rendered response text, not merely that the root item passed its own
// scope check.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// TestFSListOSDiskSymlinkNeverLeaksDenyCarvedSubtreeThroughListing is the
// real-OSDisk, real-os.Symlink end-to-end regression. "work" grants list,
// "work/private" is a nested deny carve-out, and "work/linked-private" is
// a real directory symlink to "work/private" sitting OUTSIDE the
// carve-out. fastwalk's Follow:true (internal/fsext/ls.go) crosses the
// symlink and also returns the secret file spelled as
// "work/linked-private/secret.txt" — before this fix, that alias spelling
// did not lexically match the "work/private" deny root, only the broader
// "work" grant, and was allowed through fsListOne's policy filter.
//
// The rendered proof here is the symlink's OWN name, not the nested
// filename underneath it: fastwalk hands the walk callback the symlinked
// entry's ORIGINAL (symlink-typed) os.DirEntry even while traversing
// through it (charlievieth/fastwalk's onDirEnt calls w.fn with the
// pre-traversal DirEntry, whose Type() is os.ModeSymlink), so
// createFileTree — which decides file-vs-directory purely from the
// trailing separator ls.go adds only when d.IsDir() is true — always
// misclassifies a listed directory symlink's OWN entry as a file. Given
// that, printNode's file/directory branch never recurses into that node's
// children regardless of this fix, an orthogonal, pre-existing rendering
// quirk this task does not touch. "linked-private/secret.txt" is
// therefore asserted as a sanity invariant that already held even before
// this fix (for the wrong reason); the actual before/after proof is that
// "linked-private" itself — which resolves to exactly the denied
// "work/private" root — is dropped by the scope filter after this fix and
// so never appears as a rendered leaf at all.
func TestFSListOSDiskSymlinkNeverLeaksDenyCarvedSubtreeThroughListing(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	private := filepath.Join(work, "private")
	require.NoError(t, os.MkdirAll(private, 0o755))
	secretFile := filepath.Join(private, "secret.txt")
	require.NoError(t, os.WriteFile(secretFile, []byte("topsecret"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(work, "visible.txt"), []byte("x"), 0o644))

	alias := filepath.Join(work, "linked-private")
	if err := os.Symlink(private, alias); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: work,
		Entries: []permission.FolderScopeEntry{
			{Dir: work, Ops: []permission.FileOp{permission.FileOpList}},
			{Dir: private, Ops: nil}, // nested deny carve-out over the canonical target
		},
	})
	require.NoError(t, err)

	tool := NewFSListTool(scope, work, config.ToolLs{}, nil)
	raw, err := json.Marshal(FSListParams{Items: []FSListItem{{Path: "."}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "call-1", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	require.Contains(t, resp.Content, "visible.txt", "the ungranted-adjacent grant must still list normally")
	require.NotContains(t, resp.Content, "secret.txt",
		"the denied private subtree's file must never be rendered, whether reached directly or via the followed symlink")
	require.NotContains(t, resp.Content, "linked-private",
		"the symlink alias itself resolves to the denied canonical root and must never be rendered as a listing entry")
}

// TestFSListFakeProviderAliasResultRespectsCanonicalDenyCarveOut is the
// fake-provider equivalent for fs_list: the fake's List returns a single
// entry spelled through the alias, and its EvalSymlinks resolves that
// alias to the canonically-denied "private" directory — something a real
// filepath.EvalSymlinks could never do for a path that isn't an actual
// symlink, proving fsListOne actually calls resolveScopedPath against the
// ACTIVE injected provider rather than trusting the lexical result.
func TestFSListFakeProviderAliasResultRespectsCanonicalDenyCarveOut(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	alias := filepath.Join(workingDir, "alias")
	private := filepath.Join(workingDir, "private")
	disk.putDir(alias)
	disk.putDir(private)
	secretFile := filepath.Join(private, "secret.txt")
	disk.putFile(secretFile, "topsecret")
	visibleFile := filepath.Join(workingDir, "visible.txt")
	disk.putFile(visibleFile, "x")

	// The provider's traversal names the file through its lexical ALIAS
	// spelling — exactly what OSDisk's Follow:true fastwalk would return
	// after crossing a real directory symlink.
	disk.listResult = ListResult{Entries: []string{
		visibleFile,
		filepath.Join(alias, "secret.txt"),
	}}
	disk.mu.Lock()
	disk.symlinks = map[string]string{filepath.Clean(alias): private}
	disk.mu.Unlock()

	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: workingDir,
		Entries: []permission.FolderScopeEntry{
			{Dir: workingDir, Ops: []permission.FileOp{permission.FileOpList}},
			{Dir: private, Ops: nil}, // deny carve-out over the CANONICAL target
		},
	})
	require.NoError(t, err)

	tool := NewFSListTool(scope, workingDir, config.ToolLs{}, disk)
	raw, err := json.Marshal(FSListParams{Items: []FSListItem{{Path: "."}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	require.Contains(t, resp.Content, "visible.txt")
	require.NotContains(t, resp.Content, "secret.txt",
		"the alias-spelled result must be dropped once resolved to its canonical, denied target")
	require.Contains(t, resp.Content, "(1 entries hidden by the folder scope)")
	require.Contains(t, disk.Calls(), "EvalSymlinks:"+alias,
		"fs_list must resolve each RESULT path through the injected provider, not just the requested root")

	requireRealDirEmpty(t, tmp)
}

// TestFSFindFakeProviderAliasResultRespectsCanonicalDenyCarveOut is the
// fake-provider equivalent for fs_find: FindResult.Paths documents only
// "absolute matches", not canonical ones (fs_provider.go), so a provider
// is free to return an alias spelling the same way OSDisk's fs_list does.
func TestFSFindFakeProviderAliasResultRespectsCanonicalDenyCarveOut(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	workingDir := filepath.Join(tmp, "virtual")
	disk := newFakeDisk()
	disk.putDir(workingDir)
	alias := filepath.Join(workingDir, "alias")
	private := filepath.Join(workingDir, "private")
	disk.putDir(alias)
	disk.putDir(private)
	secretFile := filepath.Join(private, "secret.txt")
	disk.putFile(secretFile, "topsecret")
	keepFile := filepath.Join(workingDir, "keep.txt")
	disk.putFile(keepFile, "x")

	disk.findResult = FindResult{Paths: []string{
		keepFile,
		filepath.Join(alias, "secret.txt"),
	}}
	disk.mu.Lock()
	disk.symlinks = map[string]string{filepath.Clean(alias): private}
	disk.mu.Unlock()

	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: workingDir,
		Entries: []permission.FolderScopeEntry{
			{Dir: workingDir, Ops: []permission.FileOp{permission.FileOpFind}},
			{Dir: private, Ops: nil}, // deny carve-out over the CANONICAL target
		},
	})
	require.NoError(t, err)

	tool := NewFSFindTool(scope, workingDir, disk)
	raw, err := json.Marshal(FSFindParams{Items: []FSFindItem{{Pattern: "**/*.txt"}}})
	require.NoError(t, err)
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c1", Input: string(raw)})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	require.Contains(t, resp.Content, "keep.txt")
	require.NotContains(t, resp.Content, "secret.txt",
		"the alias-spelled result must be dropped once resolved to its canonical, denied target")
	require.Contains(t, resp.Content, "(1 results hidden by the folder scope)")
	require.Contains(t, disk.Calls(), "EvalSymlinks:"+alias,
		"fs_find must resolve each RESULT path through the injected provider, not just the requested root")

	requireRealDirEmpty(t, tmp)
}
