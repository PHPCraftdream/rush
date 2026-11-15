package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// purgeQueuedWorkAfterStop removes the session's durable queued-work rows
// (run queue, pending injects, orphan-call outbox) after the operator has
// stopped the session, so the RunQueuePump of the NEXT rush process on the
// same data directory cannot resurrect it from a row the stopped holder left
// behind (task #1153). Best effort on purpose: kill is a rescue command that
// must keep working even when the DB/app side is stuck, so a failure here is
// a warning that names the consequence, never a command error.
//
// The DB is opened with setupAppLite, which skips recoverInterruptedTurns and
// leaves the RunQueuePump nil: a purge pass must not itself start draining
// the very queue it is about to clear.
func purgeQueuedWorkAfterStop(cmd *cobra.Command, sessionID string) {
	a, err := setupAppLite(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open the session DB to purge queued runs/injects (%v); a queued run or pending inject of session %s may be picked up by the next rush process on this data directory\n", err, sessionID)
		return
	}
	defer a.Shutdown()
	purged, err := a.Sessions.PurgeQueuedWorkForSession(cmd.Context(), sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to purge queued runs/injects of session %s: %v\n", sessionID, err)
		return
	}
	if purged.RunQueue+purged.PendingInject+purged.OrphanOutbox > 0 {
		fmt.Fprintf(os.Stderr, "purged queued work of session %s: %d run-queue, %d pending inject(s), %d orphan-outbox row(s)\n",
			sessionID, purged.RunQueue, purged.PendingInject, purged.OrphanOutbox)
	}
}
