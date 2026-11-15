// --global-dir flag plumbing: arg scanning, applyGlobalDir (env override +
// eager layout creation), and flag-beats-env precedence.
package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestGlobalDirFlagFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "absent", args: []string{"run", "x"}, want: ""},
		{name: "value form", args: []string{"--global-dir", "/tmp/gd", "run"}, want: "/tmp/gd"},
		{name: "equals form", args: []string{"run", "--global-dir=/tmp/gd"}, want: "/tmp/gd"},
		{name: "single dash value form", args: []string{"-global-dir", "/tmp/gd"}, want: "/tmp/gd"},
		{name: "single dash equals form", args: []string{"-global-dir=/tmp/gd"}, want: "/tmp/gd"},
		{name: "after end-of-flags sentinel", args: []string{"run", "--", "--global-dir", "/tmp/gd"}, want: ""},
		{name: "missing value", args: []string{"--global-dir"}, want: ""},
		{name: "value looks like another flag", args: []string{"--global-dir", "--debug"}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, globalDirFlagFromArgs(tt.args))
		})
	}
}

func TestApplyGlobalDir(t *testing.T) {
	// Keep home resolution away from the real user profile.
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("USERPROFILE", tempHome)
	t.Setenv(config.GlobalDirEnv, "")

	target := filepath.Join(tempHome, "gd-target")
	applyGlobalDir(target)

	got := os.Getenv(config.GlobalDirEnv)
	want, err := filepath.Abs(target)
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(want), got)

	for _, dir := range []string{
		got,
		filepath.Join(got, "config"),
		filepath.Join(got, "data"),
		filepath.Join(got, "cache"),
	} {
		info, err := os.Stat(dir)
		require.NoError(t, err)
		require.True(t, info.IsDir())
		if runtime.GOOS != "windows" {
			require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
		}
	}
}

func TestApplyGlobalDirRelativePath(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("USERPROFILE", tempHome)
	t.Setenv(config.GlobalDirEnv, "")

	t.Chdir(t.TempDir())
	applyGlobalDir("relative-gd-root")

	got := os.Getenv(config.GlobalDirEnv)
	require.True(t, filepath.IsAbs(got), "env value must be made absolute: %q", got)
	require.Equal(t, filepath.Clean(got), got)
	_, err := os.Stat(filepath.Join(got, "config"))
	require.NoError(t, err)
}

func TestApplyGlobalDirFlagWinsOverEnv(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("USERPROFILE", tempHome)
	t.Setenv(config.GlobalDirEnv, "")

	old := filepath.Join(t.TempDir(), "old-root")
	newRoot := filepath.Join(t.TempDir(), "new-root")
	t.Setenv(config.GlobalDirEnv, old)

	applyGlobalDir(newRoot)

	want, err := filepath.Abs(newRoot)
	require.NoError(t, err)
	require.Equal(t, filepath.Clean(want), os.Getenv(config.GlobalDirEnv))
	require.NotEqual(t, old, os.Getenv(config.GlobalDirEnv))
}

func TestApplyGlobalDirEmptyIsNoop(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv("HOME", tempHome)
	t.Setenv("USERPROFILE", tempHome)
	t.Setenv(config.GlobalDirEnv, "")

	envValue := filepath.Join(t.TempDir(), "env-only-root")
	t.Setenv(config.GlobalDirEnv, envValue)

	applyGlobalDir("")

	require.Equal(t, envValue, os.Getenv(config.GlobalDirEnv))
}
