package permission

// R5-6 follow-up regression (see r5_6_leading_slash_regression_windows_test.go
// for the Windows-only half of that finding).
//
// R14-5 naming/comment correction: this is a GENERIC, platform-
// independent property test. The hardcoded "/rush-library-mode-root"
// literal below is an arbitrary driveless absolute path used as a
// stand-in for the driveless-virtual namespace shape the R5-6-era
// sentinel had; it is NOT a claim that this literal equals the current
// sdk.LibraryVirtualRoot() on every OS -- since R6-1 that value is
// computed per-OS ("/rush-library-mode-root" on Unix,
// `K:\rush-library-mode-root` on Windows; see
// internal/agent/tools/fs_library_virtual_root.go's
// libraryVirtualRootForOS) and is genuinely filepath.IsAbs everywhere,
// so a WorkingDir in the driveless namespace can no longer arise from
// the SDK in practice. What this test pins is the defense-in-depth
// behavior BuildFolderScope deliberately kept for such a namespace
// (folderscope.go's alreadyAbsInThisNamespace case (2)): a "." entry
// joined onto a driveless-virtual WorkingDir must still grant normally
// on every OS, since "." is never itself ambiguously absolute anywhere.
// For the CURRENT sentinel value's shape, see
// r14_5_current_virtual_root_regression_test.go (package
// permission_test -- this package's own test package cannot import
// internal/agent/tools, which imports permission).

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFolderScope_DrivelessVirtualRootEntryStaysUnjoined(t *testing.T) {
	// Arbitrary driveless absolute path: a stand-in for the legacy
	// R5-6-era namespace shape, NOT the current per-OS
	// sdk.LibraryVirtualRoot() value (see the file header).
	const legacyDrivelessVirtualRoot = "/rush-library-mode-root"
	spec := FolderScopeSpec{
		WorkingDir: legacyDrivelessVirtualRoot,
		Entries: []FolderScopeEntry{
			{Dir: ".", Ops: []FileOp{FileOpRead}},
		},
	}
	scope, err := BuildFolderScope(spec)
	require.NoError(t, err)

	target := filepath.Join(legacyDrivelessVirtualRoot, "scoped", "f.txt")
	assert.NoError(t, scope.Check(target, FileOpRead),
		"a WorkingDir that is itself in the driveless-virtual namespace must still grant normally")
}
