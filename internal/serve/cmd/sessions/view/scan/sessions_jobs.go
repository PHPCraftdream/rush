package sessscan

// The `rush sessions jobs <id>` subcommand: observation-only CLI surface
// for live batch work. Lists a session's batch roots and its delegation
// tree -- what is running, its state, whether the read is complete, and
// each root's last recorded result.
//
// Deliberately observation-only: there is no job-level "kill" here. Stopping
// work on a DIFFERENT, still-live host needs the OWNING process to act;
// `rush sessions kill` kills the holder of the SESSION lock, and nothing
// holds it between turns (a host waiting for a job holds its host lock
// instead), so the only lever is stopping the host process itself -- which
// stops everything else that host runs.
import (
	"sync"

	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit"
	sessbatch "github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/report/batch"
	"github.com/spf13/cobra"
)

var sessionsJobsCmd = &cobra.Command{
	Use:   "jobs <session-id>",
	Short: "List batch work owned by a session and its delegation tree",
	Long: `List the batch roots owned by a session AND every
 descendant session reachable through a delegation --
 what batch work is running or recently finished, in which state, whether
 the read is complete, and each root's task counts and delivery debt.

 This reads the SAME durable state "sessions why"/"sessions list" consult to
 decide whether a session is still "delegating" -- it names the concrete
 work behind that verdict. Works from any process: a row is written by
 whichever process actually owns the work, not the one running this command.

 Batch work is not stopped by killing a session lock holder: use
 "rush batch stop" for a root, or stop the owning host process.`,
	Args: cobra.ExactArgs(1),
	Example: `
# Everything this session and its sub-agents are/were doing
rush sessions jobs pr-42

# Machine-readable
rush sessions jobs pr-42 --json
  `,
	RunE: sessionsJobsCmdRun,
}

var sessionsJobsFlagsOnce sync.Once

// NewSessionsJobsCmd returns a fresh `sessions jobs` command.
func NewSessionsJobsCmd() *cobra.Command {
	sessionsJobsFlagsOnce.Do(func() {
		sessionsJobsCmd.Flags().Bool("json", false, "Emit one JSON object per job")
	})
	return cmdkit.DeepFreshCmd(sessionsJobsCmd)
}

func sessionsJobsCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")

	a, err := cmdkit.SetupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, err := cmdkit.ResolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	return sessbatch.RenderSessionsJobsBatch(cmd.Context(), a, a.Config().Options.DataDirectory, sess.ID, asJSON)
}
