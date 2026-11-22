package install

// Cross-target comparison: the Codex Skills installed by
// targets.InstallCodexSkills must genuinely differ from the Claude
// slash-commands this package renders. It lives here (not in targets)
// because the real Claude renderer (claudeSlashCommandContent &
// friends) is unexported in this package — no Claude renderer is copied
// into test code. skillBodyAfterFrontmatter below is a string-trimming
// helper duplicated from targets/codex_test.go (unexported there).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/admin/install/targets"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skillBodyAfterFrontmatter(content string) string {
	content = strings.TrimPrefix(content, targets.ClaudeSlashCommandSentinel+"\n")
	if strings.HasPrefix(content, "---\n") {
		end := strings.Index(content[4:], "\n---\n")
		if end >= 0 {
			content = content[4+end+len("\n---\n"):]
		}
	}
	content = strings.TrimLeft(content, "\n")
	content = strings.TrimPrefix(content, targets.ClaudeSlashCommandSentinel+"\n\n")
	content = strings.TrimPrefix(content, "Any text you type after invoking this skill is the task — treat it exactly as `$ARGUMENTS` below would have been substituted.\n\n")
	return content
}

func TestInstalledCodexSkillsDifferFromClaudeAndReturnCompletion(t *testing.T) {
	skillsDir := t.TempDir()
	require.NoError(t, targets.InstallCodexSkills(skillsDir))

	rushClaude, err := claudeSlashCommandContent()
	require.NoError(t, err)
	fallbackClaude, err := claudeFallbackCommandContent()
	require.NoError(t, err)
	claudeContent := map[string]string{
		"rush":          rushClaude,
		"rush-fallback": fallbackClaude,
		"wrush":         claudeWrushCommandContent(),
		"wcrush":        claudeWcrushCommandContent(),
	}
	for name, claude := range claudeContent {
		codex, readErr := os.ReadFile(filepath.Join(skillsDir, name, "SKILL.md"))
		require.NoError(t, readErr)
		codexContent := string(codex)
		assert.NotEqual(t, skillBodyAfterFrontmatter(claude), skillBodyAfterFrontmatter(codexContent), "%s must have target-specific instructions", name)
		assert.NotContains(t, codexContent, "--codex-thread-id", "%s Codex variant must not mention Codex thread callbacks", name)
		assert.NotContains(t, claude, "--codex-thread-id", "%s Claude variant must not mention Codex callbacks", name)
		if name == "rush" {
			assert.NotContains(t, codexContent, "watch` is primary", "generated Codex skill must not inherit Claude/common watch-primary monitoring")
			assert.Contains(t, codexContent, "**Completion:**", "Codex-specific completion instructions must remain in Codex")
			assert.NotContains(t, claude, "**Completion:**", "Codex-specific completion instructions must not appear in Claude")
		}
	}
}
