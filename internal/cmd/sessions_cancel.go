package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/spf13/cobra"
)

var sessionsCancelCmd = &cobra.Command{
	Use:   "cancel <session-id>",
	Short: "Request cancellation of a running session",
	Long: `Signal a running rush process to cancel the given session.

Sets a database flag that the running agent checks after each step. Works
across processes — use it from a second terminal or orchestrator to stop
a ` + "`rush run`" + ` that is running in the background.

The running agent will stop within one step of the flag being set. A
` + "`rush run`" + ` loop that is waiting between turns (on a running job, a
delegation or a retry pause) re-reads the flag at least every 5 seconds and
ends the run as "canceled" (envelope, --on-finish and ended_reason as for
Ctrl-C; the jobs the run started are cancelled), instead of waiting for the
work to finish.

The flag is a ONE-SHOT request, not a state: it is cleared when it is
honoured (the run loop exits "canceled", or the running turn aborts) and when
a human-initiated turn starts on the session (a web prompt, a resumed
delegation), so it does not abort later work. A flag set on a session that
nothing is running stays set until such a turn starts; in the meantime a
background reaction turn or a "rush run" loop on that session sees it and
stops. Cancel a session that has work to stop.

With --all only sessions that have live work are flagged — a live lock, a
"rush run" driver, a running job of their own or a live delegation. Idle
sessions (a web tab with nothing running) are skipped and counted: Ctrl-C
leaves no flag behind either, and a flag on an idle session would abort its
next turn. Name a session explicitly to flag it regardless.`,
	Args: cobra.MaximumNArgs(1),
	Example: `
# Cancel a specific session
rush sessions cancel my-session-id

# Cancel every session that has live work
rush sessions cancel --all
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		all, _ := cmd.Flags().GetBool("all")

		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		ctx := cmd.Context()

		if all {
			sessions, err := a.Sessions.List(ctx)
			if err != nil {
				return fmt.Errorf("failed to list sessions: %w", err)
			}
			dataDir := a.Config().Options.DataDirectory
			count, skipped := 0, 0
			for _, s := range sessions {
				if !sessionHasLiveWork(ctx, a, dataDir, s.ID) {
					skipped++
					continue
				}
				if err := a.Sessions.RequestCancel(ctx, s.ID); err != nil {
					fmt.Fprintf(os.Stderr, "warning: failed to cancel session %s: %v\n", s.ID, err)
					continue
				}
				cancelWakeSchedules(ctx, a, s.ID)
				count++
			}
			fmt.Fprintf(os.Stderr, "cancellation requested for %d session(s); skipped %d session(s) with no live work\n", count, skipped)
			return nil
		}

		if len(args) == 0 {
			return fmt.Errorf("requires a session id or --all flag")
		}

		sess, err := resolveSessionID(ctx, a.Sessions, args[0])
		if err != nil {
			return err
		}
		if err := a.Sessions.RequestCancel(ctx, sess.ID); err != nil {
			return fmt.Errorf("failed to request cancellation: %w", err)
		}
		if n := cancelWakeSchedules(ctx, a, sess.ID); n > 0 {
			fmt.Fprintf(os.Stderr, "cancelled %d wake schedule(s) of session %s\n", n, sess.ID)
		}
		fmt.Fprintf(os.Stderr, "cancellation requested for session %s\n", sess.ID)
		return nil
	},
}

// cancelWakeSchedules cancels every ACTIVE wake schedule (once and loop) of
// the session (stage 5a): a `rush run` waiting on a one-shot timer would
// otherwise keep waiting for a timer nobody will take down. Best effort: a
// failure is a warning, never a cancel error.
func cancelWakeSchedules(ctx context.Context, a *app.App, sessionID string) int64 {
	wake := a.WakeScheduleStore()
	if wake == nil {
		return 0
	}
	n, err := wake.CancelAllForOwner(ctx, sessionID, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to cancel wake schedules of session %s: %v\n", sessionID, err)
	}
	return n
}

// sessionHasLiveWork reports whether the classifier keeps the session open:
// in turn, or live work between turns (a driver, a running own job, a live
// delegation, an open once schedule). An unreadable live-work fact counts as
// live (the codebase's convention, encoded in the classifier). dataDir is
// kept for signature stability; the classifier derives the lock path itself.
func sessionHasLiveWork(ctx context.Context, a *app.App, dataDir, sessionID string) bool {
	act, err := a.SessionActivity(ctx, sessionID)
	if err != nil {
		return true
	}
	return kindIsLive(act.Verdict.Kind)
}

func init() {
	sessionsCancelCmd.Flags().Bool("all", false, "Cancel every top-level session that has live work (idle sessions are skipped)")
}
