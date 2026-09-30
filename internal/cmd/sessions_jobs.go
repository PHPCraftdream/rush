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
// `rush sessions kill` kills the holder of the SESSION lock, and nothing
// holds it between turns (a host waiting for a job holds its host lock
// instead), so the only lever is stopping the host process itself -- which
// stops everything else that host runs. A live row on a foreign host prints
// a hint naming that process and the command that stops it instead of
// attempting an in-process stop it cannot safely perform.
import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
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
naming that host's PID and the command that stops that process -- there is no
job-level kill, and "rush sessions kill" kills the holder of the session lock,
which nothing holds between turns, so stopping the job means stopping the
WHOLE host process (every other job and session it runs stops with it). On
POSIX the hint is "kill -INT <pid>" (graceful: rush catches SIGINT, not
SIGTERM, and on the way out kills the job process groups); on Windows it is
"taskkill /F /T /PID <pid>" (forced: no cleanup runs; the process tree dies, and
so do the job children when rush joined its kill-on-close Job Object at
startup).`,
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
				item.Hint = foreignHostKillHint(item.HostPID)
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

// foreignHostKillHint is the `sessions jobs` hint for a running row on a live
// host of another process (C13): `rush sessions kill` kills the holder of the
// SESSION lock, which nothing holds between turns, so it cannot stop such a
// job; stopping the host process can, and stops everything else it runs.
func foreignHostKillHint(pid int64) string {
	return foreignHostKillHintFor(runtime.GOOS, pid)
}

// foreignHostKillHintFor: POSIX uses SIGINT because rush catches os.Interrupt
// only -- a plain SIGTERM ends it with no cleanup and leaves the job children
// (own process groups) running, while the graceful exit's CancelAll kills
// them. Windows has no signal to send: taskkill /F /T forcibly ends the
// process tree, and main.go tries to put rush in a kill-on-close Job Object, so
// the OS then also takes every job child with it; no cleanup runs. pid 0 (host row
// missing) still says so.
func foreignHostKillHintFor(goos string, pid int64) string {
	if pid <= 0 {
		return "owned by a live host on another process (PID unknown); stop that whole process to stop this job and everything else it runs"
	}
	kill, how := fmt.Sprintf("kill -INT %d", pid), "graceful; job process groups are killed on exit"
	if goos == "windows" {
		kill, how = fmt.Sprintf("taskkill /F /T /PID %d", pid), "forced; no cleanup, the process tree dies with it"
	}
	return fmt.Sprintf("owned by a live host on another process (PID %d); stopping that whole process stops this job and everything else it runs (%s): `%s`", pid, how, kill)
}
