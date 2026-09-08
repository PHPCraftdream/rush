//go:build !windows

package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLibraryVirtualRootContainmentIsCaseSensitiveOnUnix(t *testing.T) {
	root := LibraryVirtualRoot
	upper := strings.ToUpper(root)
	if upper == root {
		t.Skip("sentinel has no letters to vary in case")
	}
	require.False(t, pathIsUnderLibraryVirtualRoot(upper, root))
}
