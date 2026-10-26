package cmd

// The `sessions reset` subcommand: wipe the message history of a session
// while keeping the session row, with --force to take over a stale lock
// first.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsResetCmd = &cobra.Command{
	Use:   "reset <id>",
	Short: "Drop a session's messages but keep its id, title, and system prompt",
	Long: `Wipe the conversation history of a session while preserving the
session row itself — including its id, title, persisted system prompt,
and per-session model selection.

Useful when you want to re-run "rush run --session <same-id>" from a
clean slate without picking a new id and losing the side-channel state
(system prompt, model overrides) that you previously configured.

The wipe is a clean slate: the session's undelivered and unreacted background
job / supervision notices are voided with the messages, so
none of them reappears in the next run. A session that is still worked on is
never reset: reset refuses while a "rush run" loop drives it (even between
turns) or a background job / delegation of it is running. Stop it first
("rush sessions cancel <id>"; a hung "rush run" can be ended by its PID, and
"rush sessions kill <id>" ends the holder of the session lock during a turn).
--force kills the process holding the session lock (a turn in progress) and takes
over the lock, then applies the same check: it does not override a loop waiting
between turns or a running job that outlived the killed process.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Wipe history, keep system prompt, continue with same id
rush sessions reset pr-42
rush run --session pr-42 "try again with the fresh context"

# Reset even if a stale lock from a crashed process is in the way
rush sessions reset pr-42 --force
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		force, _ := cmd.Flags().GetBool("force")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()

		sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
		if err != nil {
			return err
		}

		// Fork patch (orchestrator UX): --force kills any process still
		// holding the session's lock and removes the lock file. Without
		// this, a reset can succeed at the DB level but a subsequent
		// `rush run --session <same>` still fails with "session is
		// already in use" because the previous holder crashed without
		// releasing.
		//
		// Uses the shared probeThenKillHolder + removeLockWithRetry
		// helpers (defined in sessions_kill.go) so kill / wait-for-death /
		// retry-remove behaves identically here and in `sessions kill`:
		// probeThenKillHolder first attempts a real OS-level lock
		// acquisition before trusting the PID recorded in the lock file,
		// so a stale/recycled PID from an already-exited holder is never
		// blindly killed. On Windows the kill (when a live holder is
		// actually found) goes through taskkill /F /T which also
		// terminates the spawned CLI subprocess tree.
		if force {
			// Use the data directory setupApp already resolved onto `a`
			// (honors --data-dir and the project's configured
			// data_directory) instead of recomputing a cwd-based guess —
			// see task #219.
			dataDir := a.Config().Options.DataDirectory
			lockPath := filepath.Join(dataDir, "locks", "session-"+sanitiseSessionIDForFilename(sess.ID)+".lock")
			fmt.Fprintf(os.Stderr, "reset --force: acquiring session lock at %s\n", lockPath)
			pid := session.ReadLockPID(lockPath)
			lk, kr, acquireErr := acquireSessionLockForReset(dataDir, sess.ID, pid, 5*time.Second)
			if acquireErr != nil {
				fmt.Fprint(os.Stderr, kr.Report)
				return fmt.Errorf("reset --force: %w", acquireErr)
			}
			fmt.Fprint(os.Stderr, kr.Report)
			// HOLD the real OS lock across the DB reset below so no concurrent
			// `rush run --session <id>` can recreate the lock at this path and
			// start writing into the session DB while the wipe is in flight.
			// The lock FILE is deliberately NOT removed: an empty lock file
			// with no held OS lock is harmless (the next acquirer reopens and
			// overwrites it; see internal/session/lock.go's Release), and
			// removing a path a live holder may be reusing is exactly the
			// two-owners bug this command must avoid.
			defer lk.Release()
		}

		// F8: the live-loop gate first -- the batch reset's atomic refusal
		// covers batch work, but a live `rush run` loop between turns
		// holds no batch root and no lock, so it must refuse exactly like
		// the legacy path did (the R8A-3 bug).
		if err := resetLiveLoopRefusal(cmd.Context(), a, sess.ID); err != nil {
			return err
		}
		// History (messages and batch rows) goes through the
		// R10c batch reset, which refuses atomically while the owner
		// has any nonterminal root.
		batchOutcome, err := batchResetSessionHistory(cmd.Context(), a, sess.ID)
		if err != nil {
			return err
		}
		return finishSessionReset(cmd, a, sess.ID, force, fmt.Sprintf(
			"batch reset: deleted %d message(s), %d batch node(s), voided %d notice(s), cancelled %d schedule(s)",
			batchOutcome.MessagesDeleted, batchOutcome.NodesDeleted, batchOutcome.NoticesVoided, batchOutcome.SchedulesCancelled))
	},
}

// finishSessionReset is the shared tail of both reset paths: zero the
// per-session usage counters, print the standard reset line plus the
// path-specific outcome summary, and purge queued work after --force.
func finishSessionReset(cmd *cobra.Command, a *app.App, sessID string, force bool, summary string) error {
	// Zero the per-session usage counters so a follow-up run starts
	// from an honest "empty context" estimate. cost_self is never
	// lowered (it is monotonic, #1130): the reset freezes the node's
	// budget instead — cost_base = subtree_spent, so the budget
	// (subtree minus base) starts from zero again.
	if err := a.Sessions.SetSummaryAndUsage(cmd.Context(), sessID, "", 0, 0); err != nil {
		return fmt.Errorf("failed to reset session counters for %s: %w", sessID, err)
	}
	if err := a.Sessions.ResetCostBase(cmd.Context(), sessID); err != nil {
		return fmt.Errorf("failed to reset session cost for %s: %w", sessID, err)
	}
	fmt.Fprintf(os.Stderr, "reset session %s (%s)\n", sessID, short(session.HashID(sessID)))
	if summary != "" {
		fmt.Fprintln(os.Stderr, summary)
	}
	// Task #1153: --force proved the previous holder dead; its queued runs
	// and injects must not survive the reset either.
	if force {
		purgeQueuedWorkAfterStop(cmd, sessID)
	}
	return nil
}

// resetRefusedError is the shared reset-refusal shape: the batch reset's
// atomic refusal and the live-loop gate render their "still being worked
// on" reason through it.
func resetRefusedError(sessionID, what string) error {
	return fmt.Errorf("session %s is still being worked on (%s); nothing was reset. "+
		"Stop it first (`rush sessions cancel %s`: a waiting rush run exits at its next wake; a hung one can be ended by its PID, shown by `rush sessions jobs %s`, "+
		"or `rush sessions kill %s` while a turn holds the session lock), then reset again "+
		"(--force only kills the process holding the session lock; it does not stop a loop between turns or running jobs)", sessionID, what, sessionID, sessionID, sessionID)
}
