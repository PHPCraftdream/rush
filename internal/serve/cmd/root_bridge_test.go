package cmd

import "github.com/spf13/cobra"

var (
	rootCmd        *cobra.Command
	executeForTest func()
)

func Execute() { executeForTest() }

func InstallRootForTest(root *cobra.Command, execute func()) {
	rootCmd = root
	executeForTest = execute
}
