package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// A relative --cwd must come back absolute: callers re-resolve the returned
// path after the chdir, which used to apply a relative one twice.
func TestResolveCwd_RelativeFlagReturnsAbsolute(t *testing.T) {
	base := t.TempDir()
	child := filepath.Join(base, "sub", "proj")
	require.NoError(t, os.MkdirAll(child, 0o755))

	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(orig) })
	require.NoError(t, os.Chdir(base))

	cmd := &cobra.Command{}
	cmd.Flags().StringP("cwd", "c", "", "")
	require.NoError(t, cmd.ParseFlags([]string{"--cwd", filepath.Join("sub", "proj")}))

	got, err := ResolveCwd(cmd)
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(got), "got %q", got)

	info, err := os.Stat(got)
	require.NoError(t, err, "returned path must exist when re-resolved from the new cwd")
	require.True(t, info.IsDir())

	now, err := os.Getwd()
	require.NoError(t, err)
	wantInfo, err := os.Stat(child)
	require.NoError(t, err)
	nowInfo, err := os.Stat(now)
	require.NoError(t, err)
	require.True(t, os.SameFile(wantInfo, nowInfo))
}
