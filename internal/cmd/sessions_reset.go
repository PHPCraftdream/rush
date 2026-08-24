package cmd

// The `sessions reset` subcommand: wipe the message history of a session
// while keeping the session row, with --force to take over a stale lock
// first.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

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
(system prompt, model overrides) that you previously configured.`,
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

		if err := a.Messages.DeleteSessionMessages(cmd.Context(), sess.ID); err != nil {
			return fmt.Errorf("failed to reset session %s: %w", sess.ID, err)
		}
		// Zero the per-session usage counters so a follow-up run starts
		// from an honest "empty context" estimate.
		//
		// Fork patch (concurrency): cost is mutated only through
		// IncrementCost now — Save no longer writes the column. Zero it
		// by applying a negative delta equal to the current value. See
		// CHANGELOG.fork.md (Section 4.I).
		previousCost := sess.Cost
		if err := a.Sessions.SetSummaryAndUsage(cmd.Context(), sess.ID, "", 0, 0); err != nil {
			return fmt.Errorf("failed to reset session counters for %s: %w", sess.ID, err)
		}
		if previousCost != 0 {
			if _, err := a.Sessions.IncrementCost(cmd.Context(), sess.ID, -previousCost); err != nil {
				return fmt.Errorf("failed to reset session cost for %s: %w", sess.ID, err)
			}
		}
		fmt.Fprintf(os.Stderr, "reset session %s (%s)\n", sess.ID, short(session.HashID(sess.ID)))
		return nil
	},
}
