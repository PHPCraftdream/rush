package cmd

import (
	"github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/models"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/ping"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/modelctl/providers"

	"github.com/spf13/cobra"
)

var (
	rootCmd        *cobra.Command
	runCmd         *cobra.Command
	worktreeCmd    *cobra.Command
	executeForTest func()
)

func Execute() { executeForTest() }

func InstallRootForTest(root *cobra.Command, execute func()) {
	templates := []*cobra.Command{
		lockCmd, unlockCmd, dirsCmd, projectsCmd, updateProvidersCmd,
		logsCmd, schemaCmd, psCmd, loginCmd, statsCmd, tokensCmd, apiCmd,
		sessionsCmd, serveCmd, models.NewModelsCmd(), ping.NewPingCmd(), ping.NewPingFastCmd(), migrateCmd,
		queueCmd, batchCmd, providers.NewProvidersCmd(), mcpCmd, claudeInitCmd,
		claudeDelCmd, cliRefreshCmd, codexInitCmd, codexDelCmd, geminiInitCmd,
		geminiDelCmd, grokInitCmd, grokDelCmd, ompInitCmd, ompDelCmd,
		opencodeInitCmd, opencodeDelCmd, qwenInitCmd, qwenDelCmd, resultCmd,
		systemPromptCmd,
	}
	for _, template := range templates {
		for _, child := range root.Commands() {
			if child.Name() == template.Name() {
				root.RemoveCommand(child)
				break
			}
		}
	}
	root.AddCommand(templates...)
	runCmd, _, _ = root.Find([]string{"run"})
	worktreeCmd, _, _ = root.Find([]string{"worktree"})
	rootCmd = root
	executeForTest = execute
}
