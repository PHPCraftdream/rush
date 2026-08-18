package cmd

// The `sessions delete` subcommand: delete a session and all its messages
// by full id or hash prefix.

import (
	"context"
	"fmt"
	"os"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsDeleteCmd = &cobra.Command{
	Use:     "delete <id>",
	Aliases: []string{"rm"},
	Short:   "Delete a session and all its messages",
	Long: `Permanently delete a session row and every message attached to it.

The <id> can be a full session id or a hash prefix (as printed by
"sessions list"). Use this to clean up scratch sessions created during
experiments — for example, the per-PR ids that "crush run --session pr-NN"
leaves behind after the work is merged.`,
	Args: cobra.ExactArgs(1),
	Example: `
crush sessions delete pr-42
crush sessions delete 8a3f0c  # match by hash prefix
  `,
	RunE: func(cmd *cobra.Command, args []string) error {
		a, err := setupApp(cmd)
		if err != nil {
			return err
		}
		defer a.Shutdown()
		return deleteSessionByIDOrHash(cmd.Context(), a, args[0])
	},
}

func deleteSessionByIDOrHash(ctx context.Context, a *app.App, idOrHash string) error {
	sess, err := resolveSessionID(ctx, a.Sessions, idOrHash)
	if err != nil {
		return err
	}
	if err := a.Sessions.Delete(ctx, sess.ID); err != nil {
		return fmt.Errorf("failed to delete session %s: %w", sess.ID, err)
	}
	fmt.Fprintf(os.Stderr, "deleted session %s (%s)\n", sess.ID, short(session.HashID(sess.ID)))
	return nil
}
