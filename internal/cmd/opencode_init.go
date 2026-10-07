// OpenCode custom-command installer for Rush delegation commands.
package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

const opencodeCommandsDir = ".opencode/commands"

var opencodeInitCmd = &cobra.Command{
	Use: "opencode-init", Short: "Install Rush delegation commands for OpenCode",
	Long: "Install /rush, /rush-fallback, /wrush and /wcrush as OpenCode Markdown commands. Defaults to ~/.config/opencode/commands; use --local or --cwd for <cwd>/.opencode/commands. Existing foreign files are preserved.",
	RunE: runOpenCodeInit,
}

func runOpenCodeInit(cmd *cobra.Command, _ []string) error {
	return runOpenCodeInitWithHome(cmd, "")
}

func runOpenCodeInitWithHome(cmd *cobra.Command, homeOverride string) error {
	global, _ := cmd.Flags().GetBool("global")
	local, _ := cmd.Flags().GetBool("local")
	localMode := local || cmd.Flags().Changed("cwd")
	if global && localMode {
		return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
	}
	var dir, cwd string
	var err error
	if localMode {
		cwd, err = ResolveCwd(cmd)
		if err != nil {
			return err
		}
		dir = filepath.Join(cwd, opencodeCommandsDir)
	} else {
		home := homeOverride
		if home == "" {
			home, err = os.UserHomeDir()
		}
		if err != nil {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		dir = filepath.Join(home, ".config", "opencode", "commands")
	}
	if err := installMarkdownCommands(dir, skillTargetOpenCode); err != nil {
		return err
	}
	checkHome := homeOverride
	if checkHome == "" {
		checkHome, _ = os.UserHomeDir()
	}
	if cwd == "" {
		cwd, err = ResolveCwd(cmd)
		if err != nil {
			return err
		}
	}
	if hasRushSkillSentinel(filepath.Join(checkHome, ".agents", "skills", "rush", "SKILL.md")) ||
		hasRushSkillSentinel(filepath.Join(cwd, ".agents", "skills", "rush", "SKILL.md")) {
		fmt.Fprintln(os.Stderr, "warning: OpenCode also loads Codex-targeted Rush skills from .agents/skills; use /rush as the OpenCode command entry point")
	}
	return nil
}

func hasRushSkillSentinel(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), claudeSlashCommandSentinel)
}

func installMarkdownCommands(dir string, target skillTarget) error {
	desc, body, err := loadSkillSource("claude_slash_command", target)
	if err != nil {
		return fmt.Errorf("rush command: %w", err)
	}
	if err := writeNativeMarkdownCommand(dir, "rush.md", desc, body); err != nil {
		return fmt.Errorf("rush command: %w", err)
	}
	_, body, err = loadSkillSource("claude_crush_fallback_command", target)
	if err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	if err := writeNativeMarkdownCommand(dir, "rush-fallback.md", "Register Rush fallback guidance", body); err != nil {
		return fmt.Errorf("rush-fallback command: %w", err)
	}
	desc, body, err = parseSlashCommandSource(claudeWrushCommandTemplate)
	if err != nil {
		return fmt.Errorf("wrush command: %w", err)
	}
	content, err := toMarkdownWrushCommandMD(target, desc, body)
	if err != nil {
		return fmt.Errorf("wrush command: %w", err)
	}
	if err := writeSentinelledFile(filepath.Join(dir, "wrush.md"), claudeSlashCommandSentinel, nativeFrontMatter(content)); err != nil {
		return fmt.Errorf("wrush command: %w", err)
	}
	desc, body, err = parseSlashCommandSource(claudeWcrushCommandTemplate)
	if err != nil {
		return fmt.Errorf("wcrush command: %w", err)
	}
	content, err = toMarkdownWcrushCommandMD(target, desc, body)
	if err != nil {
		return fmt.Errorf("wcrush command: %w", err)
	}
	if err := writeSentinelledFile(filepath.Join(dir, "wcrush.md"), claudeSlashCommandSentinel, nativeFrontMatter(content)); err != nil {
		return fmt.Errorf("wcrush command: %w", err)
	}
	return nil
}

func nativeFrontMatter(content string) string {
	content = strings.TrimPrefix(content, claudeSlashCommandSentinel+"\n")
	content = strings.TrimLeft(content, "\n")
	if !strings.HasPrefix(content, "---\n") {
		return content
	}
	parts := strings.SplitN(content[4:], "\n---\n", 2)
	if len(parts) != 2 {
		return content
	}
	return "---\n" + parts[0] + "\n---\n" + claudeSlashCommandSentinel + "\n" + strings.TrimLeft(parts[1], "\n")
}

func writeNativeMarkdownCommand(dir, name, desc, body string) error {
	content := nativeFrontMatter(renderFrontMatterMD("", desc, body, "$ARGUMENTS"))
	return writeSentinelledFile(filepath.Join(dir, name), claudeSlashCommandSentinel, content)
}

func init() {
	opencodeInitCmd.Flags().Bool("global", false, "Install globally (default).")
	opencodeInitCmd.Flags().Bool("local", false, "Install into the current project's .opencode/commands/.")
	rootCmd.AddCommand(opencodeInitCmd)
}
