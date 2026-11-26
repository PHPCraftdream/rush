package settingsctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/term"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/spf13/cobra"
)

var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

var lockCmd = &cobra.Command{
	Use:   "lock [password]",
	Short: "Protect settings or manage machine-wide command locks",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		yes, err := cmd.Flags().GetBool("yes")
		if err != nil {
			return err
		}
		if !stdinIsTerminal() && !yes {
			return errors.New("non-interactive settings locking requires --yes; use 'rush lock list' for machine-wide locks, 'rush lock run <name> -- <command>' to run under a lock, or 'rush lock --yes <password>' to protect settings")
		}
		if args[0] == "" {
			return errors.New("an argument must not be empty")
		}
		scope, err := cmdkit.ScopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := cmdkit.SetupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		if err := a.Store().LockSettings(scope, args[0]); err != nil {
			return err
		}
		name, path := "global", config.ActiveConfigFileName(config.GlobalConfigData())
		if scope == config.ScopeWorkspace {
			name = "local"
			path = config.ActiveConfigFileName(filepath.Join(a.Store().Config().Options.DataDirectory, "rush.json"))
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
		scope, err := cmdkit.ScopeFromFlags(cmd, config.ScopeGlobal)
		if err != nil {
			return err
		}
		a, err := cmdkit.SetupAppLite(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		if scope == config.ScopeGlobal {
			if state, stateErr := a.Store().LockState(); stateErr == nil && !state.Global && state.Local {
				return errors.New("only the workspace is locked; pass --local to unlock it")
			}
		}
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

// NewLockCmd returns the rush lock command (registering lock/unlock flags
// exactly once; former package init body, lane K2 of the sprint0 cmd split
// removed package init registration).
var newLockCmdOnce sync.Once

func NewLockCmd() *cobra.Command {
	newLockCmdOnce.Do(func() {
		for _, c := range []*cobra.Command{lockCmd, unlockCmd} {
			c.Flags().Bool("global", false, "Use global settings")
			c.Flags().Bool("local", false, "Use workspace settings")
		}
		lockCmd.Flags().Bool("yes", false, "Confirm settings protection in non-interactive use")
	})
	return cmdkit.DeepFreshCmd(lockCmd)
}

// NewUnlockCmd returns the rush unlock command.
func NewUnlockCmd() *cobra.Command {
	NewLockCmd()
	return cmdkit.DeepFreshCmd(unlockCmd)
}
