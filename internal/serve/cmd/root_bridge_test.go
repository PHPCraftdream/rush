package cmd

import "github.com/spf13/cobra"

var (
	rootCmd        *cobra.Command
	executeForTest func()
)

func Execute() { executeForTest() }

func InstallRootForTest(root *cobra.Command, execute func()) {
	templates := []*cobra.Command{
		lockCmd, unlockCmd, runCmd, dirsCmd, projectsCmd, updateProvidersCmd,
		logsCmd, schemaCmd, psCmd, loginCmd, statsCmd, tokensCmd, apiCmd,
		sessionsCmd, serveCmd, modelsCmd, pingCmd, pingFastCmd, migrateCmd,
		queueCmd, batchCmd, providersCmd, worktreeCmd, mcpCmd, claudeInitCmd,
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
	rootCmd = root
	executeForTest = execute
}
