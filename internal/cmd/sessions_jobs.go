package cmd

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

func init() {
	sessionsJobsCmd.Flags().Bool("json", false, "Emit one JSON object per job")
	sessionsCmd.AddCommand(sessionsJobsCmd)
}

// jobsJSONItem is the --json shape: one batch root plus its derived fields
// (held/termination verdict, debt, task counts, the awaiting-answer flag).
type jobsJSONItem struct {
	OwnerSessionID           string `json:"owner_session_id"`
	ToolCallID               string `json:"tool_call_id"`
	Kind                     string `json:"kind"`
	State                    string `json:"state"`
	Liveness                 string `json:"liveness,omitempty"` // "held" for a nonterminal root
	HostID                   string `json:"host_id"`
	HostPID                  int64  `json:"host_pid,omitempty"`
	HostLabel                string `json:"host_label,omitempty"`
	ChildSessionID           string `json:"child_session_id,omitempty"`
	Delivery                 string `json:"delivery"`
	Wake                     bool   `json:"wake"`
	Reacted                  bool   `json:"reacted"`
	CreatedAt                int64  `json:"created_at"`
	UpdatedAt                int64  `json:"updated_at"`
	ResultSummary            string `json:"result_summary,omitempty"`
	ResultIsError            bool   `json:"result_is_error,omitempty"`
	Hint                     string `json:"hint,omitempty"`
	BatchRootID              string `json:"batch_root_id,omitempty"`
	BatchHeld                bool   `json:"batch_held,omitempty"`
	BatchCancelling          bool   `json:"batch_cancelling,omitempty"`
	BatchAwaitingTermination bool   `json:"batch_awaiting_termination,omitempty"`
	BatchAwaitingAnswer      bool   `json:"batch_awaiting_answer,omitempty"`
	BatchDebt                int64  `json:"batch_debt,omitempty"`
	BatchTasksPending        int    `json:"batch_tasks_pending,omitempty"`
	BatchTasksActive         int    `json:"batch_tasks_active,omitempty"`
	BatchTasksDone           int    `json:"batch_tasks_done,omitempty"`
	BatchUnknown             bool   `json:"batch_unknown,omitempty"`
}

func sessionsJobsCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	return renderSessionsJobsBatch(cmd.Context(), a, a.Config().Options.DataDirectory, sess.ID, asJSON)
}
