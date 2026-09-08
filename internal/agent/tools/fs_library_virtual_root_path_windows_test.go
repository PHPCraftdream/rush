//go:build windows

package tools

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLibraryVirtualRootContainmentIsCaseInsensitiveOnWindows(t *testing.T) {
	root := LibraryVirtualRoot
	mixed := strings.ToLower(root)
	if mixed == root {
		mixed = strings.ToUpper(root)
	}
	require.True(t, pathIsUnderLibraryVirtualRoot(mixed, root))
	require.True(t, pathIsUnderLibraryVirtualRoot(filepath.Join(mixed, "Child"), root))
}

func TestLibraryVirtualRootContainmentRecognizesExtendedDrivePath(t *testing.T) {
	root := LibraryVirtualRoot
	extended := `\\?\` + root
	require.True(t, pathIsUnderLibraryVirtualRoot(extended, root))
	require.True(t, pathIsUnderLibraryVirtualRoot(extended+`\child`, root))
	require.False(t, pathIsUnderLibraryVirtualRoot(extended+"-sibling", root))
}
