package app

// Unit tests for fsToolsForScope (T10): the op→name mapping that keeps a
// "scoped + restricted" run's AllowTools table in step with the actual
// scoped toolset. The presence rules must mirror the coordinator's
// applyCallFolderScope exactly: one operation per tool, and fs_write
// appears when create OR overwrite is granted (its per-item check picks
// the operation by path existence). Under RestrictedRun an empty
// AllowTools table denies every plain tool, so this mapping is what
// makes "scoped + restricted" usable at all.

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func compileScopeForTest(t *testing.T, entries ...permission.FolderScopeEntry) *permission.FolderScope {
	t.Helper()
	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{
		WorkingDir: t.TempDir(),
		Entries:    entries,
	})
	require.NoError(t, err)
	return &scope
}

func TestFSToolsForScopeNilGrantsNothing(t *testing.T) {
	t.Parallel()
	assert.Empty(t, fsToolsForScope(nil))
}

func TestFSToolsForScopeGrantsMirrorOps(t *testing.T) {
	t.Parallel()
	scope := compileScopeForTest(t, permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{
		permission.FileOpRead, permission.FileOpGrep, permission.FileOpDelete,
	}})
	assert.ElementsMatch(t,
		[]string{tools.FSReadToolName, tools.FSGrepToolName, tools.FSDeleteToolName},
		fsToolsForScope(scope))
}

func TestFSToolsForScopeWriteNeedsCreateOrOverwrite(t *testing.T) {
	t.Parallel()
	createScope := compileScopeForTest(t, permission.FolderScopeEntry{
		Dir: ".",
		Ops: []permission.FileOp{permission.FileOpCreate},
	})
	assert.ElementsMatch(t, []string{tools.FSWriteToolName}, fsToolsForScope(createScope))

	overwriteScope := compileScopeForTest(t, permission.FolderScopeEntry{
		Dir: ".",
		Ops: []permission.FileOp{permission.FileOpOverwrite},
	})
	assert.ElementsMatch(t, []string{tools.FSWriteToolName}, fsToolsForScope(overwriteScope))

	listScope := compileScopeForTest(t, permission.FolderScopeEntry{
		Dir: ".",
		Ops: []permission.FileOp{permission.FileOpList},
	})
	assert.NotContains(t, fsToolsForScope(listScope), tools.FSWriteToolName)
}

func TestFSToolsForScopeCarveOutOnlyGrantsNothing(t *testing.T) {
	t.Parallel()
	// A single deny carve-out (no Ops) grants no operations at all.
	scope := compileScopeForTest(t, permission.FolderScopeEntry{Dir: "."})
	assert.Empty(t, fsToolsForScope(scope))
}
