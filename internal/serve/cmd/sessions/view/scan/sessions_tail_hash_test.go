package sessscan

// R7C-6: `sessions tail` resolved its argument (id or HASH prefix) but then
// used the raw argument for the message list and the finish check, so a HASH
// printed nothing and --follow never ended; Ctrl-C ended it with "database
// error: context canceled".

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdtest"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// runTail runs the real `sessions tail` RunE against dataDir with ctx.
func runTail(t *testing.T, ctx context.Context, dataDir, arg string, follow bool) (stdout string, err error) {
	t.Helper()
	cmd := NewSessionsTailCmd(cmdkit.SetupApp)
	ensureScanRootFlags(t, cmd, dataDir)
	require.NoError(t, cmd.Flags().Set("cwd", ""))
	require.NoError(t, cmd.Flags().Set("follow", strconv.FormatBool(follow)))
	require.NoError(t, cmd.Flags().Set("from-message", ""))
	require.NoError(t, cmd.Flags().Set("format", "text"))
	require.NoError(t, cmd.Flags().Set("with-subagents", "false"))
	cmd.SetContext(ctx)
	stdout, _ = cmdtest.CaptureStdoutAndStderr(t, func() { err = cmd.RunE(cmd, []string{arg}) })
	return stdout, err
}

// seedTailSession creates a finished session and returns its id and dataDir.
func seedTailSession(t *testing.T, finish bool) (id, dataDir string) {
	t.Helper()
	a, _, dataDir := isolatedListEnvWithConfiguredDataDir(t)
	sess, err := a.Sessions.CreateWithID(context.Background(), "tail/hash target", "tail by hash")
	require.NoError(t, err)
	user, err := a.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "fix the login timeout"}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, user.ID)
	if finish {
		cmdtest.AddFinishedAssistant(t, a.Messages, sess.ID, message.FinishReasonEndTurn)
	} else {
		_, err = a.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
			Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "still thinking"}},
		})
		require.NoError(t, err)
	}
	a.Shutdown()
	return sess.ID, dataDir
}

// Revert-check: passing the raw argument (the hash) to Messages.List prints
// nothing; passing it to tailSessionFinished never ends --follow.
func TestSessionsTailCmdRun_ByHashPrintsMessagesAndFollowEnds(t *testing.T) {
	id, dataDir := seedTailSession(t, true)
	hash := coresession.HashID(id)[:8]

	out, err := runTail(t, context.Background(), dataDir, hash, false)
	require.NoError(t, err)
	require.Contains(t, out, "fix the login timeout")
	require.Contains(t, out, "delegated the work")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err = runTail(t, ctx, dataDir, hash, true)
	require.NoError(t, err, "a finished session's --follow ends by itself")
	require.NoError(t, ctx.Err(), "and does not run into the deadline")
	require.Contains(t, out, "delegated the work")
}

// Ctrl-C (the run context cancelled) ends --follow with exit 0, as the help
// says, not with "database error: context canceled".
//
// Revert-check: without the context handling in the follow loop the cancelled
// List returns "database error: context canceled".
func TestSessionsTailCmdRun_FollowInterruptedExitsClean(t *testing.T) {
	id, dataDir := seedTailSession(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	// Interrupt once following has started: a fixed delay raced the database
	// connect on slow runners ("failed to connect to database: context
	// canceled") and could fire before anything was printed.
	tailFollowStartedHook = cancel
	t.Cleanup(func() { tailFollowStartedHook = nil })

	out, err := runTail(t, ctx, dataDir, coresession.HashID(id)[:8], true)

	require.NoError(t, err)
	require.Contains(t, out, "still thinking")
}

func ensureScanRootFlags(t *testing.T, cmd *cobra.Command, dataDir string) {
	if f := cmd.Flags().Lookup("debug"); f == nil {
		cmd.Flags().Bool("debug", false, "")
	}
	if f := cmd.Flags().Lookup("data-dir"); f == nil {
		cmd.Flags().String("data-dir", "", "")
	}
	if f := cmd.Flags().Lookup("cwd"); f == nil {
		cmd.Flags().StringP("cwd", "c", "", "")
	}
	require.NoError(t, cmd.Flags().Set("data-dir", dataDir))
}
