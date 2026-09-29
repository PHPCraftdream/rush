package cmd

// The `rush sessions jobs <id>` subcommand: observation-only CLI surface
// for the phase-4 durable async job ledger (docs/plans/2026-09-28-async-
// phase4-durable-core.md sec.5 step 7). Lists a session's OWN async_jobs
// rows plus its whole delegation tree (session.AsyncJobStore.JobsInTree,
// following child_session_id at any state, not just running) -- what is
// running, on which host, whether that host is provably alive/dead/
// unknown, and each row's last recorded result.
//
// Deliberately observation-only: there is no job-level "kill" here. Killing
// a job on a DIFFERENT, still-live host needs the OWNING process to act;
// today's only lever is `rush sessions kill <session-id>`, which stops the
// whole holder process. A live row on a foreign host prints that hint
// instead of attempting an in-process stop it cannot safely perform.
import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsJobsCmd = &cobra.Command{
	Use:   "jobs <session-id>",
	Short: "List durable async jobs (bash/run_command/agent/agentic_fetch) owned by a session and its delegation tree",
	Long: `List the durable async_jobs ledger rows owned by a session AND every
descendant session reachable through a delegation row (child_session_id) --
what async command/delegation is running or recently finished, on which
host, whether that host is provably alive/dead/unknown right now, and each
row's last recorded result summary.

This reads the SAME durable state "sessions why"/"sessions list" consult to
decide whether a session is still "delegating" -- it names the concrete
job(s) behind that verdict. Works from any process: a row is written by
whichever process actually owns the job, not the one running this command.

A 'running' row on a LIVE host owned by a DIFFERENT process prints a hint
naming that host's PID and "rush sessions kill <session-id>" -- there is no
job-level kill; stopping it means stopping the whole holder process.`,
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

// jobsJSONItem is the --json shape: the raw row plus the derived fields a
// plain db.AsyncJob can't carry (liveness verdict, host pid/label, the
// foreign-live-host hint).
type jobsJSONItem struct {
	OwnerSessionID string `json:"owner_session_id"`
	ToolCallID     string `json:"tool_call_id"`
	Kind           string `json:"kind"`
	State          string `json:"state"`
	Liveness       string `json:"liveness,omitempty"` // alive|dead|unknown; "" for a non-running row
	HostID         string `json:"host_id"`
	HostPID        int64  `json:"host_pid,omitempty"`
	HostLabel      string `json:"host_label,omitempty"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Delivery       string `json:"delivery"`
	Wake           bool   `json:"wake"`
	Reacted        bool   `json:"reacted"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
	ResultSummary  string `json:"result_summary,omitempty"`
	ResultIsError  bool   `json:"result_is_error,omitempty"`
	Hint           string `json:"hint,omitempty"`
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

	store := a.AsyncJobStore()
	if store == nil {
		if asJSON {
			return nil
		}
		fmt.Println("(no async job data available)")
		return nil
	}

	jobs, walkIncomplete := store.JobsInTree(cmd.Context(), sess.ID)
	if walkIncomplete {
		fmt.Fprintln(os.Stderr, "warning: could not fully enumerate the delegation tree; some jobs may be missing")
	}
	if len(jobs) == 0 {
		if !asJSON {
			fmt.Println("(no jobs)")
		}
		return nil
	}

	// Host pid/label lookup, cached per host id (display-only, doc sec.3.6).
	hostInfo := map[string]db.AsyncHost{}
	getHost := func(id string) (db.AsyncHost, bool) {
		if id == "" {
			return db.AsyncHost{}, false
		}
		if h, ok := hostInfo[id]; ok {
			return h, h.ID != ""
		}
		h, err := store.GetAsyncHost(cmd.Context(), id)
		if err != nil {
			hostInfo[id] = db.AsyncHost{}
			return db.AsyncHost{}, false
		}
		hostInfo[id] = h
		return h, true
	}

	items := make([]jobsJSONItem, 0, len(jobs))
	for _, j := range jobs {
		item := jobsJSONItem{
			OwnerSessionID: j.OwnerSessionID,
			ToolCallID:     j.ToolCallID,
			Kind:           j.Kind,
			State:          j.State,
			HostID:         j.HostID,
			Delivery:       j.Delivery,
			Wake:           j.Wake != 0,
			Reacted:        j.Reacted != 0,
			CreatedAt:      j.CreatedAt,
			UpdatedAt:      j.UpdatedAt,
			ResultSummary:  j.ResultSummary.String,
			ResultIsError:  j.ResultIsError.Int64 != 0,
		}
		if j.ChildSessionID.Valid {
			item.ChildSessionID = j.ChildSessionID.String
		}
		if h, ok := getHost(j.HostID); ok {
			item.HostPID = h.Pid
			item.HostLabel = h.Label
		}
		if j.State == "running" {
			status := store.HostLiveness(j.HostID)
			item.Liveness = strings.ToLower(status.String())
			if status == session.HostStatusAlive && !session.IsOwnHostID(j.HostID) {
				item.Hint = fmt.Sprintf("owned by a live host on another process (PID %d); use `rush sessions kill %s` to stop it", item.HostPID, j.OwnerSessionID)
			}
		}
		items = append(items, item)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		for _, item := range items {
			if err := enc.Encode(item); err != nil {
				return err
			}
		}
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tTOOL_CALL_ID\tKIND\tSTATE\tLIVE\tHOST\tPID\tCHILD\tDELIVERY\tWAKE\tREACTED\tAGE\tRESULT")
	now := time.Now()
	for _, item := range items {
		live := "-"
		if item.Liveness != "" {
			live = item.Liveness
		}
		pid := "-"
		if item.HostPID != 0 {
			pid = fmt.Sprintf("%d", item.HostPID)
		}
		child := "-"
		if item.ChildSessionID != "" {
			child = short(session.HashID(item.ChildSessionID))
		}
		result := truncate(item.ResultSummary, 40)
		if result == "" {
			result = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%t\t%t\t%s\t%s\n",
			short(session.HashID(item.OwnerSessionID)), item.ToolCallID, item.Kind, item.State, live,
			short(item.HostID), pid, child, item.Delivery, item.Wake, item.Reacted,
			formatDurationShort(now.Sub(time.Unix(item.UpdatedAt, 0))), result)
		if item.Hint != "" {
			fmt.Fprintf(w, "  ->\t%s\t\t\t\t\t\t\t\t\t\t\t\n", item.Hint)
		}
	}
	return w.Flush()
}
