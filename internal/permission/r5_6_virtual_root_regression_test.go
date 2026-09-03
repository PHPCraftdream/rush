package permission

// R5-6 follow-up regression (see r5_6_leading_slash_regression_windows_test.go
// for the Windows-only half of this finding). This half is genuinely
// cross-platform: a "." entry joined onto a driveless-virtual WorkingDir
// (sdk.LibraryVirtualRoot's shape) must still grant normally on every OS,
// since "." is never itself ambiguously absolute anywhere.

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildFolderScope_VirtualRootEntryStaysUnjoined(t *testing.T) {
	const virtualRoot = "/rush-library-mode-root"
	spec := FolderScopeSpec{
		WorkingDir: virtualRoot,
		Entries: []FolderScopeEntry{
			{Dir: ".", Ops: []FileOp{FileOpRead}},
		},
	}
	scope, err := BuildFolderScope(spec)
	require.NoError(t, err)

	target := filepath.Join(virtualRoot, "scoped", "f.txt")
	assert.NoError(t, scope.Check(target, FileOpRead),
		"a WorkingDir that is itself in the driveless-virtual namespace must still grant normally")
}
