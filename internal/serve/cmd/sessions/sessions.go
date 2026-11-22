package sessions

import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	sessclean "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/cleanup"
	sesslive "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/live"
	sesslocks "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/live/locks"
	sessaudit "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/report/audit"
	sessbatch "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/report/batch"
	sesscost "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/report/cost"
	sessstate "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/state"
	sessview "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/view"
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

var (
	sessionsCostCmd   = sesscost.NewSessionsCostCmd(cmdkit.SetupApp)
	sessionsDiffCmd   = sessscan.NewSessionsDiffCmd(cmdkit.SetupApp)
	sessionsGrepCmd   = sessscan.NewSessionsGrepCmd(cmdkit.SetupApp)
	sessionsAuditCmd  = sessaudit.NewSessionsAuditCmd()
	sessionsLocksCmd  = sesslocks.NewSessionsLocksCmd(cmdkit.SetupApp)
	sessionsKillCmd   = sesslocks.NewSessionsKillCmd(cmdkit.SetupApp, sessclean.PurgeQueuedWorkAfterStop, sessbatch.BatchKillLiveNote)
	sessionsReapCmd   = sesslocks.NewSessionsReapCmd(cmdkit.SetupApp)
	sessionsForkCmd   = sesslive.NewSessionsForkCmd(cmdkit.SetupApp)
	sessionsListCmd   = sessscan.NewSessionsListCmd()
	sessionsShowCmd   = sessview.NewSessionsShowCmd(cmdkit.SetupApp)
	sessionsStatusCmd = sessstate.NewSessionsStatusCmd(cmdkit.SetupApp)
	sessionsWhyCmd    = sessstate.NewSessionsWhyCmd(cmdkit.SetupApp)
	sessionsGcCmd     = sessclean.NewSessionsGcCmd(cmdkit.SetupApp)
	sessionsCancelCmd = sesslive.NewSessionsCancelCmd()
	sessionsJobsCmd   = sessscan.NewSessionsJobsCmd()
)

var newSessionsCmdOnce sync.Once

// NewSessionsCmd returns a fresh sessions command tree.
func NewSessionsCmd() *cobra.Command {
	newSessionsCmdOnce.Do(func() {
		sessionsResetCmd := sesslive.NewSessionsResetCmd()
		sessionsDeleteCmd := sessclean.NewSessionsDeleteCmd(cmdkit.SetupApp)
		sessionsTailCmd := sessscan.NewSessionsTailCmd(cmdkit.SetupApp)
		sessionsLastCmd := sessscan.NewSessionsLastCmd()
		sessionsPurgeCmd := sesslocks.NewSessionsPurgeCmd(sessclean.CompactHint)
		sessionsWatchCmd := sesslive.NewSessionsWatchCmd()
		sessionsPickCmd := sesslive.NewSessionsPickCmd()
		sessionsCacheCmd := sessstate.NewSessionsCacheCmd(cmdkit.SetupApp)
		sessionsTreeCmd := sessview.NewSessionsTreeCmd(cmdkit.SetupApp)
		sessionsCompactCmd := sessclean.NewSessionsCompactCmd()
		sessionsInjectCmd := sesslive.NewSessionsInjectCmd()
		sessionsCmd.AddCommand(
			sessionsListCmd,
			sessionsDeleteCmd,
			sessionsResetCmd,
			sessionsShowCmd,
			sessionsLocksCmd,
			sessionsTailCmd,
			sessionsLastCmd,
			sessionsStatusCmd,
			sessionsWhyCmd,
			sessionsGcCmd,
			sessionsPurgeCmd,
			sessionsKillCmd,
			sessionsReapCmd,
			sessionsWatchCmd,
			sessionsPickCmd,
			sessionsGrepCmd,
			sessionsCostCmd,
			sessionsCacheCmd,
			sessionsDiffCmd,
			sessionsCancelCmd,
			sessionsForkCmd,
			sessionsTreeCmd,
			sessionsAuditCmd,
			sessionsCompactCmd,
		)
		// Keep Inject and Jobs appended after the existing sessions command set.
		sessionsCmd.AddCommand(sessionsInjectCmd)
		sessionsCmd.AddCommand(sessionsJobsCmd)
	})
	return cmdkit.DeepFreshCmd(sessionsCmd)
}
