package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/PHPCraftdream/rush/internal/audit"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/spf13/cobra"
)

const (
	defaultAuditRetentionDays     = 30
	defaultHeartbeatRetentionDays = 7
)

// pruneAuditFiles points the audit resolver at the global data directory
// and removes journal files past the retention. When quiet, the count is
// reported by the caller (--json mode) instead of stderr text.
func pruneAuditFiles(days int, quiet bool) (int, error) {
	audit.SetDirFunc(func() string { return filepath.Dir(config.GlobalConfigData()) })
	removed, err := audit.Prune(time.Duration(days) * 24 * time.Hour)
	if err != nil {
		return 0, fmt.Errorf("prune audit files: %w", err)
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "pruned %d audit file(s)\n", removed)
	}
	return removed, nil
}

// pruneHeartbeatFiles points the heartbeat resolver at the heartbeat/
// directory next to the global settings file and removes snapshots past
// the retention. The package's prune only ever touches that directory, its
// own tmp files, and never a live process. days == 0 disables the prune.
func pruneHeartbeatFiles(days int, quiet bool) (int, error) {
	heartbeat.SetDirFunc(heartbeat.DefaultDir)
	if days == 0 {
		if !quiet {
			fmt.Fprintln(os.Stderr, "heartbeat pruning disabled (--heartbeat-days 0)")
		}
		return 0, nil
	}
	removed, err := heartbeat.Prune(time.Duration(days) * 24 * time.Hour)
	if err != nil {
		return 0, fmt.Errorf("prune heartbeat files: %w", err)
	}
	if !quiet {
		fmt.Fprintf(os.Stderr, "pruned %d heartbeat file(s)\n", removed)
	}
	return removed, nil
}

// logsPruneJSON is the --json report; every field is additive to the text
// output, which keeps its existing lines unchanged.
type logsPruneJSON struct {
	LogTruncated         bool  `json:"log_truncated"`
	LogBytesBefore       int64 `json:"log_bytes_before"`
	AuditFilesPruned     int   `json:"audit_files_pruned"`
	HeartbeatFilesPruned int   `json:"heartbeat_files_pruned"`
	HeartbeatDays        int   `json:"heartbeat_days"`
}

var logsPruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Truncate the rush log file and prune audit journals and heartbeat snapshots",
	Long: `Truncate .rush/logs/rush.log and prune audit and heartbeat files.

The log file is append-only and shared by every rush process on the
workspace (web server, rush run sessions): rush never rotates or
truncates it on its own, so on busy workspaces it can grow to hundreds
of megabytes and pruning is the only size control. This command blanks
the file in place (like logrotate's copytruncate); running processes
keep appending safely because their handles are opened with O_APPEND —
each new record lands at the end of the (now empty) file.

It also deletes audit-YYYY-MM-DD.jsonl journal files from the global
data directory; --audit-days 0 removes every journal file, otherwise
only files older than N days (default 30).

It also prunes old heartbeat snapshots from the heartbeat/ directory
next to the global settings file; --heartbeat-days 0 disables that,
otherwise only snapshots whose process is gone and whose last beat is
older than N days are removed (default 7). Live processes are never
touched, and nothing outside the heartbeat directory is deleted.`,
	Example: `
rush logs prune
rush logs prune --audit-days 0        # delete every audit journal file
rush logs prune --heartbeat-days 0    # disable heartbeat pruning
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
		heartbeatDays, err := cmd.Flags().GetInt("heartbeat-days")
		if err != nil {
			return err
		}
		if heartbeatDays < 0 {
			return fmt.Errorf("--heartbeat-days must be >= 0, got %d", heartbeatDays)
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		// The pre-existing rush.log truncation runs first and keeps its
		// messages and exit behaviour exactly as before.
		cwd, _ := cmd.Flags().GetString("cwd")
		dataDir, _ := cmd.Flags().GetString("data-dir")
		cfg, err := config.Load(cwd, dataDir, false)
		if err != nil {
			return err
		}
		p := filepath.Join(cfg.Config().Options.DataDirectory, "logs", "rush.log")
		var report logsPruneJSON
		report.HeartbeatDays = heartbeatDays
		info, err := os.Stat(p)
		switch {
		case os.IsNotExist(err):
			if !asJSON {
				fmt.Fprintln(os.Stderr, "(no log file)")
			}
		case err != nil:
			return err
		default:
			before := info.Size()
			if err := os.Truncate(p, 0); err != nil {
				return fmt.Errorf("truncate %s: %w", p, err)
			}
			report.LogTruncated, report.LogBytesBefore = true, before
			if !asJSON {
				fmt.Fprintf(os.Stderr, "pruned %s (%d bytes → 0)\n", p, before)
			}
		}
		auditRemoved, err := pruneAuditFiles(auditDays, asJSON)
		if err != nil {
			return err
		}
		report.AuditFilesPruned = auditRemoved
		hbRemoved, err := pruneHeartbeatFiles(heartbeatDays, asJSON)
		if err != nil {
			return err
		}
		report.HeartbeatFilesPruned = hbRemoved
		if asJSON {
			return json.NewEncoder(os.Stdout).Encode(report)
		}
		return nil
	},
}

func init() {
	logsPruneCmd.Flags().Int("audit-days", defaultAuditRetentionDays, "Delete audit journal files older than N days (0 = all; default 30)")
	logsPruneCmd.Flags().Int("heartbeat-days", defaultHeartbeatRetentionDays, "Delete heartbeat snapshots older than N days (0 = disable; default 7)")
	logsPruneCmd.Flags().Bool("json", false, "Emit one JSON object with the prune counts")
}
