package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

var logsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Truncate the rush log file to zero bytes",
	Long: `Truncate .rush/logs/rush.log to reclaim disk space.

The log file is append-only and shared by every rush process on the
workspace (web server, rush run sessions): rush never rotates or
truncates it on its own, so on busy workspaces it can grow to hundreds
of megabytes and pruning is the only size control. This command blanks
the file in place (like logrotate's copytruncate); running processes
keep appending safely because their handles are opened with O_APPEND —
each new record lands at the end of the (now empty) file.`,
	Example: `
rush logs prune
rush logs path    # check size before/after
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, _ := cmd.Flags().GetString("cwd")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		cfg, err := config.Load(cwd, dataDir, false)
		if err != nil {
			return err
		}
		p := filepath.Join(cfg.Config().Options.DataDirectory, "logs", "rush.log")
		info, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintln(os.Stderr, "(no log file)")
				return nil
			}
			return err
		}
		before := info.Size()
		if err := os.Truncate(p, 0); err != nil {
			return fmt.Errorf("truncate %s: %w", p, err)
		}
		fmt.Fprintf(os.Stderr, "pruned %s (%d bytes → 0)\n", p, before)
		return nil
	},
}
