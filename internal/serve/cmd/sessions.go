package cmd

// Root of the sessions command family: the parent `sessions` command, the
// shared init() that registers flags and wires every subcommand, and the
// small string helpers (short, truncate) used across the sessions_*.go
// files in this package.

import (
	"sync"
	"time"

	sesslive "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/live"
	sesscost "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/report/cost"
	sessscan "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/view/scan"
	"github.com/spf13/cobra"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "List, observe, and manage sessions — the full orchestration toolkit",
	Long: `Sessions are the unit of conversation context. The web UI and
"rush run" both create / continue them. This subcommand gives full
CLI access to the session store for scripting, orchestration, and debugging.

Core:        list (with STATUS column), show (with purpose + budget), delete, reset (--force)
Observe:     last (with timestamps), tail --follow, locks (heartbeat + budget),
             watch (live dashboard), pick (interactive TUI),
             status <id> (one-shot machine truth: process/phase/activity/children/batch),
             why <id> (the evidence behind a status), jobs <id> (durable async
             jobs of a session and its delegation tree),
             audit (anomaly scan — strictly read-only)
Search:      grep <pattern> (message text), diff <id> (files touched),
             cost [--by model|day|session] (spend breakdown),
             cache [id] (prompt-cache effectiveness)
Orchestrate: cancel <id> (graceful DB-flag stop), fork <id> [--at N],
             inject <id> (message into a session from another process),
             tree (parent-child hierarchy), gc (garbage-collect stale)
Cleanup:     purge <age> [--matching <glob>], kill <id> (force-unlock),
             reap (remove all orphan locks),
             compact (rebuild the session store: VACUUM the freelist #1161)`,
}

// NewSessionsCmd returns the rush sessionsCmd command tree, registering its flags and children
// exactly once (former package init body; lane K2 of the sprint0 cmd split
// removed package init registration). The command object itself is a package
// var that only this constructor mutates.
var sessionsCostCmd = sesscost.NewSessionsCostCmd(setupApp)

var sessionsDiffCmd = sessscan.NewSessionsDiffCmd(setupApp)

var sessionsGrepCmd = sessscan.NewSessionsGrepCmd(setupApp)

var sessionsForkCmd = sesslive.NewSessionsForkCmd(setupApp)

var newSessionsCmdOnce sync.Once

func NewSessionsCmd() *cobra.Command {
	newSessionsCmdOnce.Do(func() {
		sessionsListCmd.Flags().Bool("json", false, "Emit one JSON object per line instead of a table")

		sessionsResetCmd.Flags().Bool("force", false, "Also kill any process holding the session lock (the lock file stays); reset still refuses while the session has live work")

		sessionsShowCmd.Flags().Bool("json", false, "Emit structured JSON instead of text")
		sessionsShowCmd.Flags().Bool("with-messages", false, "Include all messages in the output")
		sessionsShowCmd.Flags().Bool("full", false, "Show full message content (implies --with-messages)")
		sessionsShowCmd.Flags().Bool("with-subagents", false, "Also render each sub-agent delegation's transcript as a demarcated block (implies --with-messages; text output)")

		sessionsLocksCmd.Flags().Bool("json", false, "Emit NDJSON (one JSON object per line)")
		sessionsLocksCmd.Flags().Bool("stale-only", false, "Filter to locks older than 10 minutes or for dead processes")
		sessionsLocksCmd.Flags().Bool("prune", false, "Remove lock files proven (via a real OS-level lock probe) to have no live holder. Off by default: `sessions locks` is read-only unless this is set.")
		sessionsLocksCmd.Flags().Bool("all", false, "Include idle lock files (no holder PID recorded, offline pulse, no live-driver evidence) that the default view collapses into a one-line summary")

		sessionsTailCmd.Flags().Bool("follow", false, "Keep polling for new messages until session finishes")
		sessionsTailCmd.Flags().String("from-message", "", "Resume from this message ID (skip earlier)")
		sessionsTailCmd.Flags().String("format", "text", "Output format: text or ndjson")
		sessionsTailCmd.Flags().Bool("with-subagents", false, "After the parent stream, render each sub-agent delegation's transcript as a demarcated block (snapshot; not re-emitted while --follow)")

		sessionsLastCmd.Flags().IntP("n", "n", 10, "Number of messages to show")
		sessionsLastCmd.Flags().String("format", "text", "Output format: text or ndjson")
		sessionsLastCmd.Flags().Bool("with-subagents", false, "After the parent messages, render each sub-agent delegation's transcript as a demarcated block")

		sessionsStatusCmd.Flags().Bool("json", false, "Emit a single JSON object instead of text")

		sessionsCacheCmd.Flags().Bool("json", false, "Emit JSON instead of a table")
		sessionsCacheCmd.Flags().String("since", "", "Only count messages newer than this: Go duration (30m, 24h), day suffix (7d), or a bare integer read as days. Aggregates across all sessions; omit the session argument.")
		sessionsCacheCmd.Flags().String("by", "", "Grouping for the cross-session view: model (default) or day")

		sessionsCmd.AddCommand(sessionsListCmd, sessionsDeleteCmd, sessionsResetCmd, sessionsShowCmd, sessionsLocksCmd, sessionsTailCmd, sessionsLastCmd, sessionsStatusCmd, sessionsWhyCmd, sessionsGcCmd, sessionsPurgeCmd, sessionsKillCmd, sessionsReapCmd, sessionsWatchCmd, sessionsPickCmd, sessionsGrepCmd, sessionsCostCmd, sessionsCacheCmd, sessionsDiffCmd, sessionsCancelCmd, sessionsForkCmd, sessionsTreeCmd, sessionsAuditCmd, sessionsCompactCmd)

		sessionsAuditCmd.Flags().Bool("all", false, "Audit every session in the database (otherwise pass a session id or a prefix of one)")
		sessionsAuditCmd.Flags().String("since", "", "Only analyse messages newer than this: Go duration (30m, 24h), day suffix (7d), or a bare integer read as days; with --all, sessions with no message in the window are not scanned")
		sessionsAuditCmd.Flags().Bool("worktrees", false, "Scan linked worktrees' databases (read-only)")
		sessionsAuditCmd.Flags().Bool("json", false, "Emit one JSON object per scanned database instead of text")

		sessionsCancelCmd.Flags().Bool("all", false, "Cancel every top-level session that has live work (idle sessions are skipped)")

		sessionsCompactCmd.Flags().Bool("dry-run", false, "Print size, freelist, estimate and gate verdicts without compacting")
		sessionsCompactCmd.Flags().Bool("force", false, "Compact despite live rush hosts or live session locks (their writers may fail with SQLITE_BUSY); never bypasses migrate.lock or a busy WAL checkpoint")
		sessionsCompactCmd.Flags().Bool("json", false, "Emit a single JSON summary object instead of text")
		sessionsCompactCmd.Flags().String("role", "worker", "Model role used for session-history compaction (smart, fast, worker, or reviewer)")

		sessionsGcCmd.Flags().Bool("dry-run", false, "Print what would be deleted without deleting")
		sessionsGcCmd.Flags().String("older-than", "7d", "Delete empty sessions older than this (e.g. 7d, 24h, 30m)")
		sessionsGcCmd.Flags().String("jobs-older-than", "", "Also purge finished, debt-free batch work older than this (e.g. 3d, 12h); empty = only the fixed 7-day retention (web server every 60s, each rush run at start and every 60s while it runs)")
		sessionsGcCmd.Flags().Int("max-sessions", 0, "Maximum number of sessions to delete (0 = unlimited)")
		sessionsGcCmd.Flags().Bool("json", false, "Emit one JSON object per deleted session (plus a job-count summary line with --jobs-older-than)")

		sessionsInjectCmd.Flags().StringP("message", "m", "", "Message text to inject (mutually exclusive with --file)")
		sessionsInjectCmd.Flags().StringP("file", "f", "", "Read message text from this file (mutually exclusive with --message)")
		sessionsInjectCmd.Flags().Bool("interrupt", false, "Cancel the current turn and restart it with this message")
		sessionsInjectCmd.Flags().Bool("json", false, "Emit a structured JSON result")

		sessionsCmd.AddCommand(sessionsInjectCmd)

		sessionsJobsCmd.Flags().Bool("json", false, "Emit one JSON object per job")
		sessionsCmd.AddCommand(sessionsJobsCmd)

		sessionsKillCmd.Flags().Bool("keep-lock", false, "Kill the process but do not delete the lock file")
		sessionsKillCmd.Flags().Duration("wait", 5*time.Second, "How long to wait for the PID to die and the OS to release the lock handle")

		sessionsPickCmd.Flags().Bool("tail", false, "Tail --follow the selected session instead of showing last N messages")

		sessionsPurgeCmd.Flags().Bool("dry-run", false, "Print what would be deleted without deleting")
		sessionsPurgeCmd.Flags().Bool("yes", false, "Skip the confirmation prompt")
		sessionsPurgeCmd.Flags().String("matching", "", "Only delete sessions whose ID matches this glob (e.g. 'garnet-sql-*')")

		sessionsReapCmd.Flags().Bool("dry-run", false, "Print what would be reclaimed without removing anything")
		sessionsReapCmd.Flags().Bool("all", false, "Legacy no-op: removal is now decided solely by the real OS-lock probe, so a lock that records a PID is reaped iff proven uncontended regardless of this flag")
		sessionsReapCmd.Flags().String("idle-older-than", "168h", "Age threshold for reclaiming idle (PID-less) lock files, e.g. 168h, 7d, 4w; \"0\" disables idle reclaim. Default 168h (7 days)")

		sessionsWatchCmd.Flags().Duration("interval", time.Second, "Poll interval for new messages (e.g. 1s, 500ms, 2s)")
	})
	return deepFreshCmd(sessionsCmd)
}
