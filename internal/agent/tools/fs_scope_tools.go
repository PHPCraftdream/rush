package tools

import (
	"slices"

	"github.com/PHPCraftdream/rush/internal/permission"
)

// FolderScopeToolOperations is the single operation-to-tool mapping used by
// the app permission gate and coordinator tool shaping.
func FolderScopeToolOperations() map[string]permission.FileOp {
	return map[string]permission.FileOp{
		FSListToolName:       permission.FileOpList,
		FSFindToolName:       permission.FileOpFind,
		FSGrepToolName:       permission.FileOpGrep,
		FSReadToolName:       permission.FileOpRead,
		FSReplaceToolName:    permission.FileOpReplace,
		FSWriteLinesToolName: permission.FileOpWriteLines,
		FSDeleteToolName:     permission.FileOpDelete,
	}
}

// FolderScopeToolNames returns the fs_* tools granted by scope. fs_write is
// included when either create or overwrite is granted.
func FolderScopeToolNames(scope permission.FolderScope) []string {
	operations := FolderScopeToolOperations()
	names := make([]string, 0, len(operations)+1)
	for name, op := range operations {
		if scope.Grants(op) {
			names = append(names, name)
		}
	}
	if scope.Grants(permission.FileOpCreate) || scope.Grants(permission.FileOpOverwrite) {
		names = append(names, FSWriteToolName)
	}
	slices.Sort(names)
	return names
}
