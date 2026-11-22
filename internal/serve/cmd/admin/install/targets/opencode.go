// OpenCode custom-command installer for Rush delegation commands.
package targets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"

	"github.com/spf13/cobra"
)

const opencodeCommandsDir = ".opencode/commands"

var opencodeInitCmd = &cobra.Command{
	Use: "opencode-init", Short: "Install Rush delegation commands for OpenCode",
	Args: cobra.NoArgs,
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
		cwd, err = cmdkit.ResolveCwd(cmd)
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
		cwd, err = cmdkit.ResolveCwd(cmd)
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
	return err == nil && strings.Contains(string(data), ClaudeSlashCommandSentinel)
}

func installMarkdownCommands(dir string, target skillTarget) error {
	desc, body, err := LoadSkillSource("claude_slash_command", target)
	if err != nil {
		return fmt.Errorf("rush command: %w", err)
	}
	if err := writeNativeMarkdownCommand(dir, "rush.md", desc, body); err != nil {
		return fmt.Errorf("rush command: %w", err)
	}
	_, body, err = LoadSkillSource("claude_crush_fallback_command", target)
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
	if err := writeSentinelledFile(filepath.Join(dir, "wrush.md"), ClaudeSlashCommandSentinel, nativeFrontMatter(content)); err != nil {
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
	if err := writeSentinelledFile(filepath.Join(dir, "wcrush.md"), ClaudeSlashCommandSentinel, nativeFrontMatter(content)); err != nil {
		return fmt.Errorf("wcrush command: %w", err)
	}
	return nil
}

func nativeFrontMatter(content string) string {
	content = strings.TrimPrefix(content, ClaudeSlashCommandSentinel+"\n")
	content = strings.TrimLeft(content, "\n")
	if !strings.HasPrefix(content, "---\n") {
		return content
	}
	parts := strings.SplitN(content[4:], "\n---\n", 2)
	if len(parts) != 2 {
		return content
	}
	return "---\n" + parts[0] + "\n---\n" + ClaudeSlashCommandSentinel + "\n" + strings.TrimLeft(parts[1], "\n")
}

func writeNativeMarkdownCommand(dir, name, desc, body string) error {
	content := nativeFrontMatter(RenderFrontMatterMD("", desc, body, "$ARGUMENTS"))
	return writeSentinelledFile(filepath.Join(dir, name), ClaudeSlashCommandSentinel, content)
}

var newOpencodeInitCmdOnce sync.Once

func NewOpencodeInitCmd() *cobra.Command {
	newOpencodeInitCmdOnce.Do(func() {
		opencodeInitCmd.Flags().Bool("global", false, "Install globally (default).")
		opencodeInitCmd.Flags().Bool("local", false, "Install into the current project's .opencode/commands/.")
	})
	return cmdkit.DeepFreshCmd(opencodeInitCmd)
}

// OpenCode command remover; only files carrying Rush's sentinel are deleted.

var opencodeDelCmd = &cobra.Command{
	Use: "opencode-del", Short: "Remove Rush delegation commands from OpenCode",
	Args: cobra.NoArgs,
	Long: "Remove Rush-owned /rush, /rush-fallback, /wrush and /wcrush commands. Defaults to ~/.config/opencode/commands; --local or --cwd targets <cwd>/.opencode/commands. Foreign files are preserved.",
	RunE: runOpenCodeDel,
}

func runOpenCodeDel(cmd *cobra.Command, _ []string) error {
	dir, err := nativeCommandDir(cmd, ".config/opencode/commands", opencodeCommandsDir)
	if err != nil {
		return err
	}
	return removeMarkdownCommands(dir)
}

func nativeCommandDir(cmd *cobra.Command, globalPath, localPath string) (string, error) {
	global, _ := cmd.Flags().GetBool("global")
	local, _ := cmd.Flags().GetBool("local")
	localMode := local || cmd.Flags().Changed("cwd")
	if global && localMode {
		return "", fmt.Errorf("--global and --local/--cwd are mutually exclusive")
	}
	if localMode {
		cwd, err := cmdkit.ResolveCwd(cmd)
		if err != nil {
			return "", err
		}
		return filepath.Join(cwd, localPath), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, filepath.FromSlash(globalPath)), nil
}

func removeMarkdownCommands(dir string) error {
	for _, name := range []string{"rush.md", "rush-fallback.md", "wrush.md", "wcrush.md"} {
		if err := removeSentinelledFile(filepath.Join(dir, name), ClaudeSlashCommandSentinel); err != nil {
			return err
		}
	}
	return nil
}

var newOpencodeDelCmdOnce sync.Once

func NewOpencodeDelCmd() *cobra.Command {
	newOpencodeDelCmdOnce.Do(func() {
		opencodeDelCmd.Flags().Bool("global", false, "Remove globally (default).")
		opencodeDelCmd.Flags().Bool("local", false, "Remove from the current project's .opencode/commands/.")
	})
	return cmdkit.DeepFreshCmd(opencodeDelCmd)
}
