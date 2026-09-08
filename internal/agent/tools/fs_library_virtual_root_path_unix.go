//go:build !windows

package tools

import (
	"path/filepath"
	"strings"
)

func pathIsUnderLibraryVirtualRoot(path, root string) bool {
	path = filepath.ToSlash(filepath.Clean(path))
	root = filepath.ToSlash(filepath.Clean(root))
	return path == root || strings.HasPrefix(path, root+"/")
}
