package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsInSourceTree(t *testing.T) {
	t.Parallel()

	t.Run("exe in dev/ with matching go.mod", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/charmbracelet/crush\n"), 0644))

		// Create dev directory and a fake exe.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("exe in .claude/worktrees/ with matching go.mod", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/charmbracelet/crush\n"), 0644))

		// Create .claude/worktrees directory and a fake exe.
		worktreesDir := filepath.Join(tmpDir, ".claude", "worktrees", "agent-x")
		require.NoError(t, os.MkdirAll(worktreesDir, 0755))
		exePath := filepath.Join(worktreesDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("exe in dev/ with no go.mod anywhere", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create dev directory and a fake exe, but no go.mod.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.False(t, IsInSourceTree(exePath))
	})

	t.Run("exe in dev/ with different module", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root with a different module.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module example.com/other\n"), 0644))

		// Create dev directory and a fake exe.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.False(t, IsInSourceTree(exePath))
	})

	t.Run("exe in bin/ with matching go.mod at root", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/charmbracelet/crush\n"), 0644))

		// Create bin directory and a fake exe.
		binDir := filepath.Join(tmpDir, "bin")
		require.NoError(t, os.Mkdir(binDir, 0755))
		exePath := filepath.Join(binDir, "crush.exe")

		// bin/ is not a marker directory, so this should NOT be detected.
		require.False(t, IsInSourceTree(exePath))
	})

	t.Run("exe in dev/ with go.mod at parent (not root)", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create a parent directory with different module.
		parentDir := filepath.Join(tmpDir, "parent")
		require.NoError(t, os.Mkdir(parentDir, 0755))
		parentGoMod := filepath.Join(parentDir, "go.mod")
		require.NoError(t, os.WriteFile(parentGoMod, []byte("module example.com/parent\n"), 0644))

		// Create dev directory under parent with crush go.mod.
		devDir := filepath.Join(parentDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0755))
		devGoMod := filepath.Join(devDir, "go.mod")
		require.NoError(t, os.WriteFile(devGoMod, []byte("module github.com/charmbracelet/crush\n"), 0644))

		// Create exe in dev/.
		exePath := filepath.Join(devDir, "crush.exe")

		// Should detect because the ancestor candidate (dev/) itself has the go.mod.
		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("quoted module name", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root with quoted module name.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte(`module "github.com/charmbracelet/crush"`+"\n"), 0644))

		// Create dev directory and a fake exe.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("worktrees marker without .claude parent", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/charmbracelet/crush\n"), 0644))

		// Create worktrees directory WITHOUT .claude parent.
		worktreesDir := filepath.Join(tmpDir, "worktrees", "agent-x")
		require.NoError(t, os.MkdirAll(worktreesDir, 0755))
		exePath := filepath.Join(worktreesDir, "crush.exe")

		// Should NOT detect because worktrees must have .claude parent.
		require.False(t, IsInSourceTree(exePath))
	})
}

func TestGuardSourceTreeRun(t *testing.T) {
	t.Parallel()

	t.Run("returns nil when exe is outside source tree", func(t *testing.T) {
		t.Parallel()
		// The test binary itself should not be in a source tree.
		err := GuardSourceTreeRun()
		require.NoError(t, err)
	})
}

func TestParseModuleFromLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "unquoted module",
			line: "module github.com/charmbracelet/crush",
			want: "github.com/charmbracelet/crush",
		},
		{
			name: "quoted module",
			line: `module "github.com/charmbracelet/crush"`,
			want: "github.com/charmbracelet/crush",
		},
		{
			name: "unquoted module with trailing space",
			line: "module github.com/charmbracelet/crush   ",
			want: "github.com/charmbracelet/crush",
		},
		{
			name: "quoted module with trailing space",
			line: `module "github.com/charmbracelet/crush"   `,
			want: "github.com/charmbracelet/crush",
		},
		{
			name: "unquoted module with leading space",
			line: "  module github.com/charmbracelet/crush",
			want: "github.com/charmbracelet/crush",
		},
		{
			name: "different module",
			line: "module example.com/other",
			want: "example.com/other",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseModuleFromLine(tt.line)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestIsInSourceTree_SelfCheck is a sanity check that the test binary itself
// is not detected as being in a source tree.
func TestIsInSourceTree_SelfCheck(t *testing.T) {
	t.Parallel()

	// Get the path to the test binary.
	exePath, err := os.Executable()
	require.NoError(t, err)

	// Resolve symlinks.
	resolvedPath, err := filepath.EvalSymlinks(exePath)
	require.NoError(t, err)

	// Convert to absolute path.
	absPath, err := filepath.Abs(resolvedPath)
	require.NoError(t, err)

	// The test binary should NOT be detected as in a source tree.
	// (It's in a temp dir or build output dir, not in dev/ or .claude/worktrees/)
	require.False(t, IsInSourceTree(absPath), "test binary should not be detected as in source tree: %s", absPath)
}

// TestIsInSourceTree_WindowsPaths tests Windows-specific path handling.
func TestIsInSourceTree_WindowsPaths(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "windows" {
		t.Skip("Windows-specific test")
	}

	t.Run("backslash path in dev/", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/charmbracelet/crush\n"), 0644))

		// Create dev directory and a fake exe using backslashes.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("backslash path in .claude\\worktrees\\", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/charmbracelet/crush\n"), 0644))

		// Create .claude\\worktrees\\ directory and a fake exe.
		worktreesDir := filepath.Join(tmpDir, ".claude", "worktrees", "agent-x")
		require.NoError(t, os.MkdirAll(worktreesDir, 0755))
		exePath := filepath.Join(worktreesDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})
}
