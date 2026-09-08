package tools

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRejectRealDiskUnderLibraryVirtualRootBoundaries(t *testing.T) {
	root := LibraryVirtualRoot

	require.Error(t, rejectRealDiskUnderLibraryVirtualRoot(OSDisk(), root))
	require.Error(t, rejectRealDiskUnderLibraryVirtualRoot(OSDisk(), filepath.Join(root, "child")))
	require.NoError(t, rejectRealDiskUnderLibraryVirtualRoot(OSDisk(), root+"-sibling"))

	// The real-disk sentinel guard must not alter a caller-owned provider.
	require.NoError(t, rejectRealDiskUnderLibraryVirtualRoot(&fakeDisk{}, filepath.Join(root, "child")))
}
