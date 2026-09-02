package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// Symlink tests below do not skip on GOOS. Instead they attempt the
// os.Symlink call and skip only if the OS refuses it: on Windows,
// creating symlinks needs developer mode or elevated privileges, which
// this repo's development machines typically have, so the test still
// runs there and skips gracefully where privileges are missing.

func TestResolveScopedPath_SymlinkEscapeResolvesOutside(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	inside := filepath.Join(tmp, "root", "inside")
	secret := filepath.Join(tmp, "secret")
	require.NoError(t, os.MkdirAll(inside, 0o755))
	require.NoError(t, os.MkdirAll(secret, 0o755))
	target := filepath.Join(secret, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("x"), 0o644))
	real := filepath.Join(inside, "real.txt")
	require.NoError(t, os.WriteFile(real, []byte("x"), 0o644))
	if err := os.Symlink(target, filepath.Join(inside, "link.txt")); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	resolved, err := resolveScopedPath(context.Background(), OSDisk(), filepath.Join(tmp, "root"), filepath.Join("inside", "link.txt"))
	require.NoError(t, err)
	wantOutside, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	require.Equal(t, wantOutside, resolved)

	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: tmp,
		Entries: []permission.FolderScopeEntry{{
			Dir: filepath.Join(tmp, "root"),
			Ops: []permission.FileOp{permission.FileOpRead},
		}},
	})
	require.NoError(t, err)

	err = scope.Check(resolved, permission.FileOpRead)
	require.Error(t, err)
	var denied *permission.ScopeDeniedError
	require.ErrorAs(t, err, &denied)

	wantInside, err := filepath.EvalSymlinks(real)
	require.NoError(t, err)
	require.NoError(t, scope.Check(wantInside, permission.FileOpRead))
}

func TestResolveScopedPath_CreateJudgedByResolvedParent(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	base := filepath.Join(tmp, "base")
	elsewhere := filepath.Join(tmp, "elsewhere")
	require.NoError(t, os.MkdirAll(base, 0o755))
	require.NoError(t, os.MkdirAll(elsewhere, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(elsewhere, "file.txt"), []byte("x"), 0o644))
	if err := os.Symlink(elsewhere, filepath.Join(base, "link")); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	resolved, err := resolveScopedPath(context.Background(), OSDisk(), base, filepath.Join("link", "newfile.txt"))
	require.NoError(t, err)
	evalElsewhere, err := filepath.EvalSymlinks(elsewhere)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(evalElsewhere, "newfile.txt"), resolved)
}

func TestResolveScopedPath_FileComponentDenies(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(base, "afile.txt"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(base, "sub"), 0o755))

	_, err := resolveScopedPath(context.Background(), OSDisk(), base, filepath.Join("afile.txt", "sub", "deeper.txt"))
	require.Error(t, err)

	_, err = resolveScopedPath(context.Background(), OSDisk(), base, filepath.Join("afile.txt"))
	require.NoError(t, err)

	_, err = resolveScopedPath(context.Background(), OSDisk(), base, filepath.Join("sub", "newfile.txt"))
	require.NoError(t, err)
}

func TestResolveScopedPath_AbsoluteAndRelativeInputs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	existing := filepath.Join(root, "existing")
	require.NoError(t, os.MkdirAll(existing, 0o755))
	file := filepath.Join(existing, "file.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	wantFile, err := filepath.EvalSymlinks(file)
	require.NoError(t, err)
	wantExisting, err := filepath.EvalSymlinks(existing)
	require.NoError(t, err)

	// Relative input.
	resolved, err := resolveScopedPath(context.Background(), OSDisk(), root, filepath.Join("existing", "file.txt"))
	require.NoError(t, err)
	require.Equal(t, wantFile, resolved)

	// Absolute input passes through SmartJoin unchanged.
	resolved, err = resolveScopedPath(context.Background(), OSDisk(), root, file)
	require.NoError(t, err)
	require.Equal(t, wantFile, resolved)

	// Relative input with traversal cleans to the same path.
	resolved, err = resolveScopedPath(context.Background(), OSDisk(), root, filepath.Join("existing", "..", "existing", "file.txt"))
	require.NoError(t, err)
	require.Equal(t, wantFile, resolved)

	// Non-existent create path is appended to the resolved parent.
	resolved, err = resolveScopedPath(context.Background(), OSDisk(), root, filepath.Join("existing", "newfile.txt"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(wantExisting, "newfile.txt"), resolved)
}
