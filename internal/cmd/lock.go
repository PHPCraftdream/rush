package cmd

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var lockCmd = &cobra.Command{
	Use:   "lock [password]",
	Short: "Protect settings",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] == "" {
			return errors.New("an argument must not be empty")
		}
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		if err := a.Store().LockSettings(scope, args[0]); err != nil {
			return err
		}
		name, path := "global", config.GlobalConfigData()
		if scope == config.ScopeWorkspace {
			name = "local"
			path = filepath.Join(a.Store().Config().Options.DataDirectory, "rush.json")
		}
		fmt.Printf("settings locked (%s) — %s\n", name, path)
		fmt.Println("unlock with: rush unlock [--local] <password>   (or pass --password <password> to a command to change settings)")
		return nil
	},
}

var unlockCmd = &cobra.Command{
	Use:   "unlock [password]",
	Short: "Remove settings protection",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if args[0] == "" {
			return errors.New("an argument must not be empty")
		}
		scope, err := scopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := setupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		if err := a.Store().UnlockSettings(scope, args[0]); err != nil {
			return err
		}
		name := "global"
		if scope == config.ScopeWorkspace {
			name = "local"
		}
		fmt.Printf("settings unlocked (%s)\n", name)
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{lockCmd, unlockCmd} {
		c.Flags().Bool("global", false, "Use global settings")
		c.Flags().Bool("local", false, "Use workspace settings")
	}
}
