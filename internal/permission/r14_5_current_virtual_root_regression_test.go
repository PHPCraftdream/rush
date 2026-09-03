package permission_test

// R14-5 addition, paired with r5_6_virtual_root_regression_test.go (the
// generic driveless-namespace property test): the same "." entry
// behavior pinned against the REAL, CURRENT sentinel value --
// tools.LibraryVirtualRoot (what sdk.LibraryVirtualRoot() re-exports),
// which since R6-1 is computed per-OS ("/rush-library-mode-root" on
// Unix, `K:\rush-library-mode-root` on Windows) and satisfies the
// platform-native filepath.IsAbs everywhere. Whatever OS this test
// runs on, it proves a "." FolderScope entry joined onto that actual
// per-OS value still grants normally.
//
// This lives in the external permission_test package because
// internal/agent/tools imports internal/permission: the internal test
// package (package permission) cannot import tools without an import
// cycle, while an external _test package can. BuildFolderScope and
// FolderScope.Check are pure string processing (no disk I/O -- see
// Check's doc comment), so the assertion needs no real K: drive (or
// root-owned "/" directory) to exist.

import (
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFolderScope_CurrentLibraryVirtualRootEntryStaysUnjoined(t *testing.T) {
	virtualRoot := tools.LibraryVirtualRoot
	require.NotEmpty(t, virtualRoot)
	require.True(t, filepath.IsAbs(virtualRoot),
		"tools.LibraryVirtualRoot must satisfy the platform-native filepath.IsAbs on this OS, got %q", virtualRoot)

	spec := permission.FolderScopeSpec{
		WorkingDir: virtualRoot,
		Entries: []permission.FolderScopeEntry{
			{Dir: ".", Ops: []permission.FileOp{permission.FileOpRead}},
		},
	}
	scope, err := permission.BuildFolderScope(spec)
	require.NoError(t, err)

	target := filepath.Join(virtualRoot, "scoped", "f.txt")
	assert.NoError(t, scope.Check(target, permission.FileOpRead),
		"a \".\" entry joined onto the CURRENT per-OS sentinel WorkingDir %q must still grant normally", virtualRoot)
}
