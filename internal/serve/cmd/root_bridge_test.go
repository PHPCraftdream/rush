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
	templates := []*cobra.Command{sessionsCmd, models.NewModelsCmd(), ping.NewPingCmd(), ping.NewPingFastCmd(), providers.NewProvidersCmd()}
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
