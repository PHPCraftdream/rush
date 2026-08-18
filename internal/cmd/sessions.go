package cmd

// Root of the sessions command family: the parent `sessions` command, the
// shared init() that registers flags and wires every subcommand, and the
// small string helpers (short, truncate) used across the sessions_*.go
// files in this package.

import (
	"github.com/spf13/cobra"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List, observe, and manage sessions — the full orchestration toolkit",
	Long: `Sessions are the unit of conversation context. The web UI and
"crush run" both create / continue them. This subcommand gives full
CLI access to the session store for scripting, orchestration, and debugging.

Core:        list (with STATUS column), show (with purpose + budget), delete, reset (--force)
Observe:     last (with timestamps), tail --follow, locks (heartbeat + budget),
             watch (live dashboard), pick (interactive TUI)
Search:      grep <pattern> (message text), diff <id> (files touched),
             cost [--by model|day|session] (spend breakdown)
Orchestrate: cancel <id> (graceful DB-flag stop), fork <id> [--at N],
             tree (parent-child hierarchy), gc (garbage-collect stale)
Cleanup:     purge <age> [--matching <glob>], kill <id> (force-unlock),
             reap (remove all orphan locks)`,
}

func init() {
	sessionsListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")

	sessionsResetCmd.Flags().Bool("force", false, "Also kill any process holding the session lock and remove the lock file")

	sessionsShowCmd.Flags().Bool("json", false, "Emit structured JSON instead of text")
	sessionsShowCmd.Flags().Bool("with-messages", false, "Include all messages in the output")
	sessionsShowCmd.Flags().Bool("full", false, "Show full message content (implies --with-messages)")
	sessionsShowCmd.Flags().Bool("with-subagents", false, "Also render each sub-agent delegation's transcript as a demarcated block (implies --with-messages; text output)")

	sessionsLocksCmd.Flags().Bool("json", false, "Emit NDJSON (one JSON object per line)")
	sessionsLocksCmd.Flags().Bool("stale-only", false, "Filter to locks older than 10 minutes or for dead processes")
	sessionsLocksCmd.Flags().Bool("prune", false, "Remove lock files proven (via a real OS-level lock probe) to have no live holder. Off by default: `sessions locks` is read-only unless this is set.")

	sessionsTailCmd.Flags().Bool("follow", false, "Keep polling for new messages until session finishes")
	sessionsTailCmd.Flags().String("from-message", "", "Resume from this message ID (skip earlier)")
	sessionsTailCmd.Flags().String("format", "text", "Output format: text or ndjson")
	sessionsTailCmd.Flags().Bool("with-subagents", false, "After the parent stream, render each sub-agent delegation's transcript as a demarcated block (snapshot; not re-emitted while --follow)")

	sessionsLastCmd.Flags().IntP("n", "n", 10, "Number of messages to show")
	sessionsLastCmd.Flags().String("format", "text", "Output format: text or ndjson")
	sessionsLastCmd.Flags().Bool("with-subagents", false, "After the parent messages, render each sub-agent delegation's transcript as a demarcated block")

	sessionsCacheCmd.Flags().Bool("json", false, "Emit JSON instead of a table")
	sessionsCacheCmd.Flags().String("since", "", "Only count messages newer than this: Go duration (30m, 24h), day suffix (7d), or a bare integer read as days. Aggregates across all sessions; omit the session argument.")
	sessionsCacheCmd.Flags().String("by", "", "Grouping for the cross-session view: model (default) or day")

	sessionsCmd.AddCommand(sessionsListCmd, sessionsDeleteCmd, sessionsResetCmd, sessionsShowCmd, sessionsLocksCmd, sessionsTailCmd, sessionsLastCmd, sessionsWhyCmd, sessionsGcCmd, sessionsPurgeCmd, sessionsKillCmd, sessionsReapCmd, sessionsWatchCmd, sessionsPickCmd, sessionsGrepCmd, sessionsCostCmd, sessionsCacheCmd, sessionsDiffCmd, sessionsCancelCmd, sessionsForkCmd, sessionsTreeCmd)
	rootCmd.AddCommand(sessionsCmd)
}

func short(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
