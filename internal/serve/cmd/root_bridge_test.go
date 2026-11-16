package cmd

import "github.com/spf13/cobra"

var (
	rootCmd        *cobra.Command
	executeForTest func()
)

func Execute() { executeForTest() }

func InstallRootForTest(root *cobra.Command, execute func()) {
	templates := []*cobra.Command{runCmd, tokensCmd, apiCmd, serveCmd, modelsCmd}
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
