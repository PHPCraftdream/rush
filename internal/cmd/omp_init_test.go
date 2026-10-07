// Revert-check manifest: TestOMPInitScopesAndConflicts → omp_init.go:20-35, opencode_del.go:32-50 and ResolveCwd; TestOMPInitCommandsAndGuidance → opencode_init.go:75-132, multi_cli_convert.go:383-426, claude_slash_command.yaml block 1 (rush.md target fallback wording), claude_crush_fallback_command.yaml blocks 1-2 (rush-fallback single limitation), multi_cli_convert.go ompWrushLaunchGuidance (wrush absolute redirect); TestOMPInitRefreshOwnership → multi_cli_convert.go:327-354; TestOMPDelOwnedFilesIdempotent → omp_del.go:18-23, opencode_del.go:32-59, multi_cli_convert.go:350-379; TestOMPSourcesRejectMarkersForAllTargets → multi_cli_convert.go:101-145.
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// Revert check: runOMPInit and runOMPDel resolve default/global/local/cwd scopes and reject conflicts.
func TestOMPInitScopesAndConflicts(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(orig) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{}, filepath.Join(home, ".omp", "agent", "commands")},
		{[]string{"--global"}, filepath.Join(home, ".omp", "agent", "commands")},
		{[]string{"--local"}, filepath.Join(cwd, ompCommandsDir)},
		{[]string{"--cwd", cwd}, filepath.Join(cwd, ompCommandsDir)},
	} {
		cmd := nativeCommandForTest(tc.args)
		if containsFlag(tc.args, "--local") || containsFlag(tc.args, "--cwd") {
			_ = cmd.Flags().Set("cwd", cwd)
		}
		require.NoError(t, runOMPInit(cmd, nil))
		for _, name := range nativeCommandNames {
			require.FileExists(t, filepath.Join(tc.want, name))
		}
	}
	require.ErrorContains(t, runOMPInit(nativeCommandForTest([]string{"--global", "--local"}), nil), "mutually exclusive")
	cmd := nativeCommandForTest([]string{"--global", "--cwd", cwd})
	_ = cmd.Flags().Set("cwd", cwd)
	require.ErrorContains(t, runOMPInit(cmd, nil), "mutually exclusive")
}

// Revert check: installMarkdownCommands, nativeFrontMatter, and converters emit native parseable Markdown commands.
func TestOMPInitCommandsAndGuidance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".omp", "commands")
	require.NoError(t, installMarkdownCommands(dir, skillTargetOmp))
	for _, name := range nativeCommandNames {
		b, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		got := string(b)
		assert.True(t, strings.HasPrefix(got, "---\n"))
		assert.Contains(t, got, claudeSlashCommandSentinel)
		assert.Contains(t, got, "description:")
		assert.Contains(t, got, "$ARGUMENTS")
		assert.NotContains(t, got, "{{RUSH_")
		assert.NotContains(t, got, "exec_command")
		assert.NotContains(t, got, "run_in_background")
		assert.NotContains(t, got, "TaskOutput")
		if name == "rush.md" {
			assert.Contains(t, got, `"async": true`)
			assert.Contains(t, got, `"timeout": 0`)
			assert.Contains(t, got, `"pty": false`)
			assert.Contains(t, got, `"cwd"`)
			assert.NotContains(t, got, "switch to that agent immediately and silently")
			assert.Contains(t, got, "cannot arm an automatic switch")
		}
		fields := strings.SplitN(strings.TrimPrefix(got, "---\n"), "\n---\n", 2)
		require.Len(t, fields, 2)
		var header map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(fields[0]), &header))
	}
	fallback, err := os.ReadFile(filepath.Join(dir, "rush-fallback.md"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(fallback), "does not arm it"))
	assert.Equal(t, 1, strings.Count(string(fallback), "route it through OMP's `task` tool"))
	wrush, err := os.ReadFile(filepath.Join(dir, "wrush.md"))
	require.NoError(t, err)
	assert.Contains(t, string(wrush), ompWrushLaunchGuidance)
	assert.Contains(t, string(wrush), "PRIMARY checkout's ABSOLUTE")
	assert.Contains(t, string(wrush), "`rush.md` file in this same directory")
	wcrush, err := os.ReadFile(filepath.Join(dir, "wcrush.md"))
	require.NoError(t, err)
	assert.Contains(t, string(wcrush), ompWcrushBackgroundGuidance)
	assert.Contains(t, strings.Join(strings.Fields(string(wcrush)), " "), "`wrush.md` file in this same directory")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 4)
	_, claudeBody, err := loadSkillSource("claude_slash_command", skillTargetClaude)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(strings.Fields(claudeBody), " "), "switch to that agent immediately and silently")
}

// Revert check: writeSentinelledFile refreshes owned commands and refuses foreign-file overwrite.
func TestOMPInitRefreshOwnership(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rush.md")
	require.NoError(t, writeNativeMarkdownCommand(dir, "rush.md", "before", "body"))
	require.NoError(t, writeNativeMarkdownCommand(dir, "rush.md", "after", "body"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "after")
	require.NoError(t, os.WriteFile(path, []byte("foreign"), 0o644))
	_ = captureStderr(t, func() { require.NoError(t, writeNativeMarkdownCommand(dir, "rush.md", "changed", "body")) })
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "foreign", string(data))
}

// Revert check: runOMPDel and removeMarkdownCommands delete only Rush-owned files and are repeatable.
func TestOMPDelOwnedFilesIdempotent(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(orig) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	global := filepath.Join(home, ".omp", "agent", "commands")
	local := filepath.Join(cwd, ompCommandsDir)
	require.NoError(t, installMarkdownCommands(global, skillTargetOmp))
	require.NoError(t, installMarkdownCommands(local, skillTargetOmp))
	foreign := filepath.Join(local, "foreign.md")
	require.NoError(t, os.WriteFile(foreign, []byte("foreign"), 0o644))
	cmd := nativeCommandForTest([]string{"--local"})
	_ = cmd.Flags().Set("cwd", cwd)
	require.NoError(t, runOMPDel(cmd, nil))
	require.NoError(t, runOMPDel(cmd, nil))
	for _, name := range nativeCommandNames {
		assert.NoFileExists(t, filepath.Join(local, name))
	}
	assert.FileExists(t, foreign)
	foreignNamed := filepath.Join(local, "rush.md")
	require.NoError(t, os.WriteFile(foreignNamed, []byte("foreign rush"), 0o644))
	cmd = nativeCommandForTest([]string{"--local"})
	_ = cmd.Flags().Set("cwd", cwd)
	require.NoError(t, runOMPDel(cmd, nil))
	assert.FileExists(t, foreignNamed)
	cmd = nativeCommandForTest([]string{"--global"})
	require.NoError(t, runOMPDel(cmd, nil))
	for _, name := range nativeCommandNames {
		assert.NoFileExists(t, filepath.Join(global, name))
	}
	require.ErrorContains(t, runOMPDel(nativeCommandForTest([]string{"--global", "--local"}), nil), "mutually exclusive")
	cmd = nativeCommandForTest([]string{"--global", "--cwd", cwd})
	_ = cmd.Flags().Set("cwd", cwd)
	require.ErrorContains(t, runOMPDel(cmd, nil), "mutually exclusive")
}

// Revert check: parseStructuredSkillSource rejects unresolved markers for all supported target variants.
func TestOMPSourcesRejectMarkersForAllTargets(t *testing.T) {
	for _, target := range []skillTarget{skillTargetCommon, skillTargetClaude, skillTargetCodex, skillTargetGemini, skillTargetGrok, skillTargetQwen, skillTargetOpenCode, skillTargetOmp} {
		for _, source := range []string{"claude_slash_command", "claude_crush_fallback_command"} {
			_, body, err := loadSkillSource(source, target)
			require.NoError(t, err)
			assert.NotContains(t, body, "{{RUSH_", "%s/%s", source, target)
		}
	}
}

func nativeCommandForTest(args []string) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("global", false, "")
	cmd.Flags().Bool("local", false, "")
	cmd.Flags().String("cwd", "", "")
	_ = cmd.ParseFlags(args)
	return cmd
}

func containsFlag(args []string, flag string) bool {
	for _, arg := range args {
		if arg == flag || strings.HasPrefix(arg, flag+"=") {
			return true
		}
	}
	return false
}
