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
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create dev directory and a fake exe.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("exe in .claude/worktrees/ with matching go.mod", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create .claude/worktrees directory and a fake exe.
		worktreesDir := filepath.Join(tmpDir, ".claude", "worktrees", "agent-x")
		require.NoError(t, os.MkdirAll(worktreesDir, 0o755))
		exePath := filepath.Join(worktreesDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("exe in dev/ with no go.mod anywhere", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create dev directory and a fake exe, but no go.mod.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.False(t, IsInSourceTree(exePath))
	})

	t.Run("exe in dev/ with different module", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root with a different module.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module example.com/other\n"), 0o644))

		// Create dev directory and a fake exe.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.False(t, IsInSourceTree(exePath))
	})

	t.Run("exe in bin/ with matching go.mod at root", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create bin directory and a fake exe.
		binDir := filepath.Join(tmpDir, "bin")
		require.NoError(t, os.Mkdir(binDir, 0o755))
		exePath := filepath.Join(binDir, "crush.exe")

		// bin/ is not a marker directory, so this should NOT be detected.
		require.False(t, IsInSourceTree(exePath))
	})

	t.Run("exe in dev/ with go.mod at parent (not root)", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create a parent directory with different module.
		parentDir := filepath.Join(tmpDir, "parent")
		require.NoError(t, os.Mkdir(parentDir, 0o755))
		parentGoMod := filepath.Join(parentDir, "go.mod")
		require.NoError(t, os.WriteFile(parentGoMod, []byte("module example.com/parent\n"), 0o644))

		// Create dev directory under parent with crush go.mod.
		devDir := filepath.Join(parentDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		devGoMod := filepath.Join(devDir, "go.mod")
		require.NoError(t, os.WriteFile(devGoMod, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

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
		require.NoError(t, os.WriteFile(goModPath, []byte(`module "github.com/PHPCraftdream/rush"`+"\n"), 0o644))

		// Create dev directory and a fake exe.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("worktrees marker without .claude parent", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create worktrees directory WITHOUT .claude parent.
		worktreesDir := filepath.Join(tmpDir, "worktrees", "agent-x")
		require.NoError(t, os.MkdirAll(worktreesDir, 0o755))
		exePath := filepath.Join(worktreesDir, "crush.exe")

		// Should NOT detect because worktrees must have .claude parent.
		require.False(t, IsInSourceTree(exePath))
	})

	// P3-5(a): Stray foreign go.mod inside dev/ should NOT stop the walk
	t.Run("stray foreign go.mod inside dev/", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root (crush module).
		rootGoMod := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(rootGoMod, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create dev/ with a foreign go.mod.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		devGoMod := filepath.Join(devDir, "go.mod")
		require.NoError(t, os.WriteFile(devGoMod, []byte("module example.com/stray\n"), 0o644))

		// Create exe in dev/.
		exePath := filepath.Join(devDir, "crush.exe")

		// Should detect because the marker dir's own go.mod is foreign, so walk continues up.
		require.True(t, IsInSourceTree(exePath))
	})

	// P3-5(b): Worktrees marker without .claude parent but with crush go.mod at root
	t.Run("worktrees without .claude parent but with crush go.mod", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// NO go.mod at tmpDir (no crush repo at parent level).
		// Create home directory (no go.mod).
		homeDir := filepath.Join(tmpDir, "home")
		require.NoError(t, os.Mkdir(homeDir, 0o755))

		// Create .claude/worktrees/wt/ with crush go.mod.
		worktreesDir := filepath.Join(homeDir, ".claude", "worktrees", "wt")
		require.NoError(t, os.MkdirAll(worktreesDir, 0o755))
		wtGoMod := filepath.Join(worktreesDir, "go.mod")
		require.NoError(t, os.WriteFile(wtGoMod, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create exe in worktrees/.
		exePath := filepath.Join(worktreesDir, "crush.exe")

		// Should detect because walk starts at worktrees marker dir itself.
		require.True(t, IsInSourceTree(exePath))
	})

	// P3-5(c): Case-insensitive marker directory "Dev"
	t.Run("case-insensitive marker Dev", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root (crush module).
		rootGoMod := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(rootGoMod, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create directory literally named "Dev" (with capital D).
		devDir := filepath.Join(tmpDir, "Dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		exePath := filepath.Join(devDir, "crush.exe")

		// Should detect because marker comparison is case-insensitive.
		require.True(t, IsInSourceTree(exePath))
	})

	// P3-5(d1): go.mod with tab separator
	t.Run("go.mod with tab separator", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root with tab after "module".
		rootGoMod := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(rootGoMod, []byte("module\tgithub.com/PHPCraftdream/rush\n"), 0o644))

		// Create dev directory and a fake exe.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		exePath := filepath.Join(devDir, "crush.exe")

		// Should detect because readModuleLine now accepts tab separator.
		require.True(t, IsInSourceTree(exePath))
	})

	// P3-5(d2): Empty go.mod (no module line) at marker dir continues up
	t.Run("empty go.mod at marker continues up", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root (crush module).
		rootGoMod := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(rootGoMod, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create dev/ with a go.mod that has no module line.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		devGoMod := filepath.Join(devDir, "go.mod")
		require.NoError(t, os.WriteFile(devGoMod, []byte("// nothing\n"), 0o644))

		// Create exe in dev/.
		exePath := filepath.Join(devDir, "crush.exe")

		// Should detect because marker dir's go.mod has no module line (returns ""), walk continues up.
		require.True(t, IsInSourceTree(exePath))
	})

	// Preserve false-positive-avoidance: different-module go.mod ABOVE dev/ should stop
	t.Run("different go.mod above dev/ stops walk", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root (crush module).
		rootGoMod := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(rootGoMod, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create interposed/ with different module.
		interposedDir := filepath.Join(tmpDir, "interposed")
		require.NoError(t, os.Mkdir(interposedDir, 0o755))
		interposedGoMod := filepath.Join(interposedDir, "go.mod")
		require.NoError(t, os.WriteFile(interposedGoMod, []byte("module example.com/other\n"), 0o644))

		// Create dev/ under interposed/.
		devDir := filepath.Join(interposedDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		exePath := filepath.Join(devDir, "crush.exe")

		// Should NOT detect because foreign go.mod at interposed/ (above dev/) stops the walk.
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
			line: "module github.com/PHPCraftdream/rush",
			want: "github.com/PHPCraftdream/rush",
		},
		{
			name: "quoted module",
			line: `module "github.com/PHPCraftdream/rush"`,
			want: "github.com/PHPCraftdream/rush",
		},
		{
			name: "unquoted module with trailing space",
			line: "module github.com/PHPCraftdream/rush   ",
			want: "github.com/PHPCraftdream/rush",
		},
		{
			name: "quoted module with trailing space",
			line: `module "github.com/PHPCraftdream/rush"   `,
			want: "github.com/PHPCraftdream/rush",
		},
		{
			name: "unquoted module with leading space",
			line: "  module github.com/PHPCraftdream/rush",
			want: "github.com/PHPCraftdream/rush",
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

// P3-5(d): Test readModuleLine with tab separator and no module line
func TestReadModuleLine(t *testing.T) {
	t.Parallel()

	t.Run("tab separator", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module\tgithub.com/PHPCraftdream/rush\n"), 0o644))

		line, err := readModuleLine(goModPath)
		require.NoError(t, err)
		require.Equal(t, "module\tgithub.com/PHPCraftdream/rush", line)
	})

	t.Run("no module line returns empty string", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("// nothing\n"), 0o644))

		line, err := readModuleLine(goModPath)
		require.NoError(t, err) // Should return ("", nil), not os.ErrNotExist
		require.Equal(t, "", line)
	})
}

// P3-4: Test that the guard message has the correct format
func TestSourceTreeGuardMessage(t *testing.T) {
	t.Parallel()

	msg := sourceTreeGuardMessage("/path/to/crush.exe")

	// Should NOT contain the old go install command
	require.NotContains(t, msg, "go install github.com/PHPCraftdream/rush")
	// Should contain the new npm package name
	require.Contains(t, msg, "@phpcraftdream/crush")
	// Should contain deploy.go
	require.Contains(t, msg, "deploy.go")
	// Should still contain "build from source"
	require.Contains(t, msg, "build from source")
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
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create dev directory and a fake exe using backslashes.
		devDir := filepath.Join(tmpDir, "dev")
		require.NoError(t, os.Mkdir(devDir, 0o755))
		exePath := filepath.Join(devDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})

	t.Run("backslash path in .claude\\worktrees\\", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()

		// Create go.mod at root.
		goModPath := filepath.Join(tmpDir, "go.mod")
		require.NoError(t, os.WriteFile(goModPath, []byte("module github.com/PHPCraftdream/rush\n"), 0o644))

		// Create .claude\\worktrees\\ directory and a fake exe.
		worktreesDir := filepath.Join(tmpDir, ".claude", "worktrees", "agent-x")
		require.NoError(t, os.MkdirAll(worktreesDir, 0o755))
		exePath := filepath.Join(worktreesDir, "crush.exe")

		require.True(t, IsInSourceTree(exePath))
	})
}
