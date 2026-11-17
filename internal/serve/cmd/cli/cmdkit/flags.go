package cmdkit

import (
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

func ScopeFromFlags(cmd *cobra.Command, def config.Scope) (config.Scope, error) {
	global, _ := cmd.Flags().GetBool("global")
	local, _ := cmd.Flags().GetBool("local")
	switch {
	case global && local:
		return def, fmt.Errorf("--global and --local are mutually exclusive")
	case global:
		return config.ScopeGlobal, nil
	case local:
		return config.ScopeWorkspace, nil
	default:
		return def, nil
	}
}

// cmdFlagString reads a string flag, returning "" when the command has
// no such flag — used for best-effort attribution (process start log
// record) on commands that may or may not define the flag.
func CmdFlagString(cmd *cobra.Command, name string) string {
	v, _ := cmd.Flags().GetString(name)
	return v
}
