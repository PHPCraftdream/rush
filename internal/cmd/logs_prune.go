package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/PHPCraftdream/rush/internal/audit"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

const defaultAuditRetentionDays = 30

// pruneAuditFiles points the audit resolver at the global data directory
// and removes journal files past the retention.
func pruneAuditFiles(days int) error {
	audit.SetDirFunc(func() string { return filepath.Dir(config.GlobalConfigData()) })
	removed, err := audit.Prune(time.Duration(days) * 24 * time.Hour)
	if err != nil {
		return fmt.Errorf("prune audit files: %w", err)
	}
	fmt.Fprintf(os.Stderr, "pruned %d audit file(s)\n", removed)
	return nil
}

var logsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Truncate the rush log file and prune audit journals",
	Long: `Truncate .rush/logs/rush.log and prune the audit journal.

The log file is append-only and shared by every rush process on the
workspace (web server, rush run sessions): rush never rotates or
truncates it on its own, so on busy workspaces it can grow to hundreds
of megabytes and pruning is the only size control. This command blanks
the file in place (like logrotate's copytruncate); running processes
keep appending safely because their handles are opened with O_APPEND —
each new record lands at the end of the (now empty) file.

It also deletes audit-YYYY-MM-DD.jsonl journal files from the global
data directory; --audit-days 0 removes every journal file, otherwise
only files older than N days (default 30).`,
	Example: `
rush logs prune
rush logs prune --audit-days 0   # delete every audit journal file
rush logs path    # check size before/after
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		auditDays, err := cmd.Flags().GetInt("audit-days")
		if err != nil {
			return err
		}
		if auditDays < 0 {
			return fmt.Errorf("--audit-days must be >= 0, got %d", auditDays)
		}
		// The pre-existing rush.log truncation runs first and keeps its
		// messages and exit behaviour exactly as before.
		cwd, _ := cmd.Flags().GetString("cwd")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		cfg, err := config.Load(cwd, dataDir, false)
		if err != nil {
			return err
		}
		p := filepath.Join(cfg.Config().Options.DataDirectory, "logs", "rush.log")
		info, err := os.Stat(p)
		switch {
		case os.IsNotExist(err):
			fmt.Fprintln(os.Stderr, "(no log file)")
		case err != nil:
			return err
		default:
			before := info.Size()
			if err := os.Truncate(p, 0); err != nil {
				return fmt.Errorf("truncate %s: %w", p, err)
			}
			fmt.Fprintf(os.Stderr, "pruned %s (%d bytes → 0)\n", p, before)
		}
		if err := pruneAuditFiles(auditDays); err != nil {
			return err
		}
		return nil
	},
}

func init() {
	logsPruneCmd.Flags().Int("audit-days", defaultAuditRetentionDays, "Delete audit journal files older than N days (0 = all; default 30)")
}
