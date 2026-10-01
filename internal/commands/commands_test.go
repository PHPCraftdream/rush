package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadFromSource_NonExistentDir(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "does-not-exist")

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Empty(t, cmds)

	// directory must NOT have been created
	_, statErr := os.Stat(dir)
	require.True(t, os.IsNotExist(statErr))
}

func TestLoadFromSource_ExistingDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.md"), []byte("say hello"), 0o644))

	cmds, err := loadFromSource(commandSource{path: dir, prefix: userCommandPrefix})
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, "user:hello", cmds[0].ID)
	require.Equal(t, "say hello", cmds[0].Content)
}

func TestLoadAll_MixedSources(t *testing.T) {
	t.Parallel()

	existing := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(existing, "cmd.md"), []byte("content"), 0o644))

	missing := filepath.Join(t.TempDir(), "nope")

	cmds, err := loadAll([]commandSource{
		{path: existing, prefix: userCommandPrefix},
		{path: missing, prefix: projectCommandPrefix},
	})
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	require.Equal(t, "user:cmd", cmds[0].ID)
}

// TestLoadProjectSkillCommands_IncludesClaudeSkillsCommands is the revert
// oracle for task #1156: command-facing loading must keep scanning other
// tools' project directories (.claude/skills), so skills installed by other
// agent tools stay available as slash commands even though the agent system
// prompt no longer advertises them. Swapping config.ProjectSkillsDir for
// the ProjectPromptSkillsDirs variant in commands.go (or shrinking
// projectSkillSubdirs) turns this test red.
func TestLoadProjectSkillCommands_IncludesClaudeSkillsCommands(t *testing.T) {
	t.Parallel()

	wd := t.TempDir()
	skillDir := filepath.Join(wd, ".claude", "skills", "optin")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: optin\ndescription: Skill from another tool that must stay available as a command.\nuser-invocable: true\n---\nBody.\n"),
		0o644,
	))

	cmds := LoadProjectSkillCommands(wd)
	require.Len(t, cmds, 1)
	require.Equal(t, "project:optin", cmds[0].ID)
	require.NotNil(t, cmds[0].Skill)
}

// TestLoadInvocableSkillsFromDir_ClaudeStyleDir composes the user-facing
// half: a Claude-style global directory (~/.claude/skills) still yields
// user: commands. Together with TestGlobalSkillsDirs_KeepOtherToolDirsForCommands
// (which guards that globalSkillsDirs keeps ~/.claude/skills) this covers
// the user: path compositionally — GlobalSkillsDirs itself cannot be
// redirected in-process because home.Dir() caches at package init, so no
// env trick is attempted here.
func TestLoadInvocableSkillsFromDir_ClaudeStyleDir(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	skillDir := filepath.Join(tmp, ".claude", "skills", "optin")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: optin\ndescription: Skill from another tool that must stay available as a command.\nuser-invocable: true\n---\nBody.\n"),
		0o644,
	))

	cmds := loadInvocableSkillsFromDir(filepath.Join(tmp, ".claude", "skills"), userCommandPrefix)
	require.Len(t, cmds, 1)
	require.Equal(t, "user:optin", cmds[0].ID)
	require.NotNil(t, cmds[0].Skill)
}
