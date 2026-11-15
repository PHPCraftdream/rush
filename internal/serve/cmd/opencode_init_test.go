// Revert-check manifest: TestOpenCodeInitScopesAndConflict → runOpenCodeInitWithHome, ResolveCwd; TestOpenCodeInitWritesValidCommandsAndHarnessGuidance → installMarkdownCommands, nativeFrontMatter, loadSkillSource; TestOpenCodeInitRefreshAndForeignPreservation → writeNativeMarkdownCommand, writeSentinelledFile; TestOpenCodeInitSkillWarningScopesDeduplicate → hasRushSkillSentinel; TestOpenCodeDelScopesOwnershipAndIdempotence → runOpenCodeDel, removeMarkdownCommands; TestOpenCodeSourcesRejectMarkersAcrossTargets → parseStructuredSkillSource.
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

// Revert check: runOpenCodeInit and runOpenCodeDel scope flags and target directories.
func TestOpenCodeInitScopesAndConflict(t *testing.T) {
	for _, scope := range []string{"default", "global", "local", "cwd"} {
		t.Run(scope, func(t *testing.T) {
			home, cwd, explicit := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Chdir(cwd)
			args := map[string][]string{"default": nil, "global": {"--global"}, "local": {"--local"}, "cwd": {"--cwd", explicit}}[scope]
			cmd := nativeTestCommand(args, explicit)
			if scope == "cwd" {
				_ = cmd.Flags().Set("cwd", explicit)
			}
			require.NoError(t, runOpenCodeInitWithHome(cmd, home))
			dest := filepath.Join(home, ".config", "opencode", "commands")
			switch scope {
			case "local":
				dest = filepath.Join(cwd, ".opencode", "commands")
			case "cwd":
				dest = filepath.Join(explicit, ".opencode", "commands")
			}
			assertOpenCodeFiles(t, dest)
			for _, other := range []string{filepath.Join(home, ".config", "opencode", "commands"), filepath.Join(cwd, ".opencode", "commands"), filepath.Join(explicit, ".opencode", "commands")} {
				if other != dest {
					assertOpenCodeFilesAbsent(t, other)
				}
			}
		})
	}
	t.Run("conflicting flags", func(t *testing.T) {
		home, explicit := t.TempDir(), t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		require.ErrorContains(t, runOpenCodeInitWithHome(nativeTestCommand([]string{"--global", "--local"}, explicit), home), "mutually exclusive")
		cmd := nativeTestCommand([]string{"--global", "--cwd", explicit}, explicit)
		_ = cmd.Flags().Set("cwd", explicit)
		require.ErrorContains(t, runOpenCodeInitWithHome(cmd, home), "mutually exclusive")
		assertOpenCodeFilesAbsent(t, filepath.Join(home, ".config", "opencode", "commands"))
		assertOpenCodeFilesAbsent(t, filepath.Join(explicit, ".opencode", "commands"))
	})
}

func assertOpenCodeFiles(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"rush.md", "rush-fallback.md", "wrush.md", "wcrush.md"} {
		require.FileExists(t, filepath.Join(dir, name))
	}
}

func assertOpenCodeFilesAbsent(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"rush.md", "rush-fallback.md", "wrush.md", "wcrush.md"} {
		assert.NoFileExists(t, filepath.Join(dir, name))
	}
}

// Revert check: installMarkdownCommands writes frontmatter/ownership and nativeFrontMatter moves the sentinel after YAML.
func TestOpenCodeInitWritesValidCommandsAndHarnessGuidance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".opencode", "commands")
	require.NoError(t, installMarkdownCommands(dir, skillTargetOpenCode))
	for _, name := range nativeCommandNames {
		data, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err)
		got := string(data)
		assert.True(t, strings.HasPrefix(got, "---\n"))
		assert.Contains(t, got, claudeSlashCommandSentinel)
		assert.Contains(t, got, "description:")
		assert.Contains(t, got, "$ARGUMENTS")
		assert.NotContains(t, got, "{{RUSH_")
		assert.NotContains(t, got, "exec_command")
		assert.NotContains(t, got, "run_in_background")
		assert.NotContains(t, got, "TaskOutput")
		if name == "rush.md" {
			assert.Contains(t, got, "timeout: 36000000")
			assert.Contains(t, got, "MILLISECONDS")
			assert.NotContains(t, strings.Join(strings.Fields(got), " "), "switch to that agent immediately and silently")
			assert.Contains(t, strings.Join(strings.Fields(got), " "), "cannot arm an automatic switch")
		}
		fm := strings.SplitN(strings.TrimPrefix(got, "---\n"), "\n---\n", 2)
		require.Len(t, fm, 2)
		var header map[string]any
		require.NoError(t, yaml.Unmarshal([]byte(fm[0]), &header))
	}
	fallback, err := os.ReadFile(filepath.Join(dir, "rush-fallback.md"))
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(fallback), "does not arm it"))
	assert.Equal(t, 1, strings.Count(string(fallback), "route it through OpenCode's `Task` tool"))
	wrush, err := os.ReadFile(filepath.Join(dir, "wrush.md"))
	require.NoError(t, err)
	assert.Contains(t, string(wrush), opencodeWrushLaunchGuidance)
	assert.Contains(t, string(wrush), "PRIMARY checkout's ABSOLUTE")
	assert.Contains(t, string(wrush), "`rush.md` file in this same directory")
	wcrush, err := os.ReadFile(filepath.Join(dir, "wcrush.md"))
	require.NoError(t, err)
	assert.Contains(t, string(wcrush), opencodeWcrushBackgroundGuidance)
	assert.Contains(t, strings.Join(strings.Fields(string(wcrush)), " "), "`wrush.md` file in this same directory")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 4)
	_, claudeBody, err := loadSkillSource("claude_slash_command", skillTargetClaude)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(strings.Fields(claudeBody), " "), "switch to that agent immediately and silently")
}

// Revert check: writeNativeMarkdownCommand and writeSentinelledFile refresh owned files but preserve foreign files.
func TestOpenCodeInitRefreshAndForeignPreservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rush.md")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(path, []byte(claudeSlashCommandSentinel+" old"), 0o644))
	require.NoError(t, writeNativeMarkdownCommand(dir, "rush.md", "new", "body $ARGUMENTS"))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "new")
	require.NoError(t, os.WriteFile(path, []byte("foreign"), 0o644))
	_ = captureStderr(t, func() { require.NoError(t, writeNativeMarkdownCommand(dir, "rush.md", "changed", "body")) })
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "foreign", string(data))
}

// Revert check: runOpenCodeInit and hasRushSkillSentinel emit one warning only for owned global/local Codex skills.
func TestOpenCodeInitSkillWarningScopesDeduplicate(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(orig) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	global := filepath.Join(home, ".agents", "skills", "rush", "SKILL.md")
	local := filepath.Join(cwd, ".agents", "skills", "rush", "SKILL.md")
	for _, tc := range []struct {
		name, content string
		want          int
	}{
		{"missing", "", 0}, {"foreign", "foreign", 0}, {"global", claudeSlashCommandSentinel, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.content != "" {
				require.NoError(t, os.MkdirAll(filepath.Dir(global), 0o755))
				require.NoError(t, os.WriteFile(global, []byte(tc.content), 0o644))
			} else {
				_ = os.Remove(global)
			}
			cmd := nativeTestCommand([]string{"--cwd", cwd}, cwd)
			_ = cmd.Flags().Set("cwd", cwd)
			stderr := captureStderr(t, func() { require.NoError(t, runOpenCodeInitWithHome(cmd, home)) })
			warnings := warningLines(stderr)
			assert.Len(t, warnings, tc.want)
			if tc.want == 1 {
				assert.Contains(t, warnings[0], "OpenCode also loads Codex-targeted Rush skills")
			}
		})
	}
	_ = os.Remove(global)
	for _, content := range []string{"foreign", claudeSlashCommandSentinel} {
		require.NoError(t, os.MkdirAll(filepath.Dir(local), 0o755))
		require.NoError(t, os.WriteFile(local, []byte(content), 0o644))
		cmd := nativeTestCommand([]string{"--cwd", cwd}, cwd)
		_ = cmd.Flags().Set("cwd", cwd)
		stderr := captureStderr(t, func() { require.NoError(t, runOpenCodeInitWithHome(cmd, home)) })
		want := 0
		if content == claudeSlashCommandSentinel {
			want = 1
		}
		warnings := warningLines(stderr)
		assert.Len(t, warnings, want)
	}
	// Both owned scopes still produce exactly one warning line.
	require.NoError(t, os.WriteFile(global, []byte(claudeSlashCommandSentinel), 0o644))
	cmd := nativeTestCommand([]string{"--cwd", cwd}, cwd)
	_ = cmd.Flags().Set("cwd", cwd)
	stderr := captureStderr(t, func() { require.NoError(t, runOpenCodeInitWithHome(cmd, home)) })
	warnings := warningLines(stderr)
	assert.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "OpenCode also loads Codex-targeted Rush skills")
}

// Revert check: runOpenCodeDel selects the same scope and removeMarkdownCommands is sentinel-only/idempotent.
func TestOpenCodeDelScopesOwnershipAndIdempotence(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(orig) })
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, dir := range []string{filepath.Join(home, ".config", "opencode", "commands"), filepath.Join(cwd, ".opencode", "commands")} {
		require.NoError(t, installMarkdownCommands(dir, skillTargetOpenCode))
	}
	foreign := filepath.Join(cwd, ".opencode", "commands", "rush.md")
	require.NoError(t, os.WriteFile(foreign, []byte("foreign"), 0o644))
	cmd := nativeTestCommand([]string{"--local"}, cwd)
	_ = cmd.Flags().Set("cwd", cwd)
	require.NoError(t, runOpenCodeDel(cmd, nil))
	require.NoError(t, runOpenCodeDel(cmd, nil))
	for _, name := range []string{"rush-fallback.md", "wrush.md", "wcrush.md"} {
		assert.NoFileExists(t, filepath.Join(filepath.Dir(foreign), name))
	}
	foreignAfter, err := os.ReadFile(foreign)
	require.NoError(t, err)
	assert.Equal(t, "foreign", string(foreignAfter))
	for _, name := range nativeCommandNames {
		assert.FileExists(t, filepath.Join(home, ".config", "opencode", "commands", name))
	}
	cmd = nativeTestCommand([]string{"--global"}, cwd)
	require.NoError(t, runOpenCodeDel(cmd, nil))
	for _, name := range nativeCommandNames {
		assert.NoFileExists(t, filepath.Join(home, ".config", "opencode", "commands", name))
	}
	cmd = nativeTestCommand([]string{"--global", "--cwd", cwd}, cwd)
	_ = cmd.Flags().Set("cwd", cwd)
	require.ErrorContains(t, runOpenCodeDel(cmd, nil), "mutually exclusive")
}

func TestFallbackPolicyRenderedForEveryNonClaudeTarget(t *testing.T) {
	for _, target := range []skillTarget{skillTargetCodex, skillTargetGemini, skillTargetGrok, skillTargetQwen, skillTargetOpenCode, skillTargetOmp} {
		t.Run(string(target), func(t *testing.T) {
			_, got, err := loadSkillSource("claude_slash_command", target)
			require.NoError(t, err)
			normalized := strings.Join(strings.Fields(got), " ")
			assert.Contains(t, normalized, "cannot arm an automatic switch", target)
			assert.Contains(t, normalized, "report the exact error, the provider, and any retry time", target)
			assert.NotContains(t, normalized, "switch to that agent immediately and silently", target)
		})
	}
	_, claude, err := loadSkillSource("claude_slash_command", skillTargetClaude)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(strings.Fields(claude), " "), "switch to that agent immediately and silently")
}

// Revert check: parseStructuredSkillSource rejects markers for each current target dialect.
func TestOpenCodeSourcesRejectMarkersAcrossTargets(t *testing.T) {
	for _, target := range []skillTarget{skillTargetCommon, skillTargetClaude, skillTargetCodex, skillTargetGemini, skillTargetGrok, skillTargetQwen, skillTargetOpenCode, skillTargetOmp} {
		for _, name := range []string{"claude_slash_command", "claude_crush_fallback_command"} {
			_, body, err := loadSkillSource(name, target)
			require.NoError(t, err, "%s/%s", name, target)
			assert.NotContains(t, body, "{{RUSH_", "%s/%s", name, target)
		}
	}
}

var nativeCommandNames = []string{"rush.md", "rush-fallback.md", "wrush.md", "wcrush.md"}

func warningLines(output string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "warning:") {
			lines = append(lines, line)
		}
	}
	return lines
}

func nativeTestCommand(args []string, _ string) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("global", false, "")
	cmd.Flags().Bool("local", false, "")
	cmd.Flags().String("cwd", "", "")
	_ = cmd.ParseFlags(args)
	return cmd
}
