//go:build windows

package tools

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenRegularFileRejectsWindowsNonregularPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, err := openRegularOSFile(context.Background(), filepath.Join(dir, "missing-parent", "file"))
	require.Error(t, err)
	_, err = openRegularOSFile(context.Background(), dir)
	require.Error(t, err)
	_, err = openRegularOSFile(context.Background(), "NUL")
	require.Error(t, err, "reserved device paths must fail closed before any read")
}
