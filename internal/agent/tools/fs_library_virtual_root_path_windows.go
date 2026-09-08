//go:build windows

package tools

import (
	"path/filepath"
	"strings"
)

func pathIsUnderLibraryVirtualRoot(path, root string) bool {
	path = normalizeWindowsLibraryPath(path)
	root = normalizeWindowsLibraryPath(root)
	return path == root || strings.HasPrefix(path, root+`\`)
}

func normalizeWindowsLibraryPath(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	const extendedPrefix = `\\?\`
	if strings.HasPrefix(path, extendedPrefix) {
		path = path[len(extendedPrefix):]
	}
	path = filepath.Clean(path)
	path = strings.ReplaceAll(path, "/", `\`)
	return strings.ToLower(path)
}
