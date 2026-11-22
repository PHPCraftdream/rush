package sessbatch

// Batch read side of the four session status commands (R12-B part 1):
// `sessions jobs/why/show/list` read live batch node
// rows through the R10c session read APIs (BatchLiveReader +
// BatchHistorySnapshot). Reads are conservative
// (R10a rule): an incomplete or failed read is reported as UNKNOWN, never as
// absence -- a command must never print "idle"/no-work while a read is
// incomplete. A cancelling root is held, nonterminal work: "stopped but
// awaiting termination".

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/PHPCraftdream/rush/internal/app"
	cliout "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	session "github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionfacts "github.com/PHPCraftdream/rush/internal/session/state/sessionfacts"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// Compatibility aliases keep the renderers on their existing names while the
// shared fact contract lives in sessions/sesskit.
type (
	batchRootFact     = sesskit.BatchRootFact
	batchSessionFacts = sesskit.BatchSessionFacts
)

func batchRootHeld(state string) bool { return sesskit.BatchRootHeld(state) }

func batchStatusAnnotation(f batchSessionFacts) string {
	return sesskit.BatchStatusAnnotation(f)
}

func reduceBatchSessionFacts(ctx context.Context, reader *session.BatchLiveReader, store *session.BatchStore, q *db.Queries, ids []string) map[string]batchSessionFacts {
	return sesskit.ReduceBatchSessionFacts(ctx, reader, store, q, ids)
}

func unavailableBatchFacts(ids []string) map[string]batchSessionFacts {
	return sesskit.UnavailableBatchFacts(ids)
}

func batchRuntimeFacts(ctx context.Context, a *app.App, dataDir string, ids []string) map[string]batchSessionFacts {
	return sesskit.BatchRuntimeFacts(ctx, a, dataDir, ids)
}

// renderBatchQuestionNote prints the awaiting-answer section of `sessions
// why`: the generic conservative note plus, when the bound notice's frame
// parsed, the child that asked and how the orchestrating agent answers it
// in-process via the inject_agent tool (no operator relay line).
func renderBatchQuestionNote(f batchSessionFacts, out io.Writer) {
	if !f.HasQuestion {
		return
	}
	fmt.Fprintln(out, "note: a held batch question is awaiting an answer")
	if f.QuestionChildID == "" && f.QuestionText == "" {
		return
	}
	fmt.Fprintf(out, "  waiting for your answer: child %s asked: %s\n", f.QuestionChildID, f.QuestionText)
	fmt.Fprintf(out, "  answer: the orchestrating agent must call `inject_agent` with child_session_id=%q and its answer as message.\n", f.QuestionChildID)
}

// renderBatchRootsForWhy renders the `sessions why` batch section from
// already-read facts: one line per root (identity, state, held/termination
// verdict, debt), then the conservative unknown line when the read was
// incomplete.
func renderBatchRootsForWhy(f batchSessionFacts, out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Batch roots:")
	if len(f.Roots) == 0 {
		if f.Unknown {
			fmt.Fprintln(out, "  (batch read incomplete; live batch work unknown)")
		} else {
			fmt.Fprintln(out, "  (none)")
		}
		return
	}
	for _, r := range f.Roots {
		verdict := r.State
		switch {
		case r.Unknown:
			verdict = "unknown (incomplete read)"
		case r.AwaitingTermination:
			verdict = r.State + " (stopped but awaiting termination)"
		case r.Held:
			verdict = r.State + " (held)"
		}
		awaiting := ""
		if r.AwaitingAnswer {
			awaiting = ", awaiting delivery/answer"
		}
		fmt.Fprintf(out, "  root %s tool call %s (owner %s): %s, debt %d, tasks %d pending/%d running/%d done%s\n",
			cliout.Short(r.RootID), r.ToolCallID, cliout.Short(coresession.HashID(r.Owner)), verdict, r.Debt,
			r.Tasks.Pending, r.Tasks.Active, r.Tasks.Completed+r.Tasks.Failed+r.Tasks.Cancelled+r.Tasks.TimedOut+r.Tasks.Interrupted+r.Tasks.Skipped, awaiting)
	}
	if f.Unknown {
		fmt.Fprintln(out, "  note: the batch read was incomplete; treat live batch work as unknown")
	}
}

// ExplainWhyBatch is the `sessions why` body: batch facts plus the
// session lock fact decide the status; no legacy async_jobs read runs.
//
// printStallHeader is the running-status renderer owned by the sessstate
// package (the stall view); it is injected as a parameter so this package
// stays downward of its consumers.
func ExplainWhyBatch(ctx context.Context, a *app.App, dataDir, sessionID string, out io.Writer, printStallHeader func(ctx context.Context, a *app.App, dataDir, sessionID, status string, lock sessionfacts.LockFact, out io.Writer) bool) error {
	facts := batchRuntimeFacts(ctx, a, dataDir, []string{sessionID})
	f := facts[sessionID]
	activity, activityErr := a.SessionActivity(ctx, sessionID)
	if activityErr != nil {
		return fmt.Errorf("failed to classify session %s: %w", sessionID, activityErr)
	}
	lock := activity.Facts.Lock
	// The message read feeds both the honest at-rest reason below and the
	// "Last assistant message" section -- one read, no second List.
	msgs, msgsErr := a.Messages.List(ctx, sessionID)
	lastFinish := ""
	if msgsErr == nil {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == message.Assistant {
				if fin := msgs[i].FinishPart(); fin != nil {
					lastFinish = string(fin.Reason)
				}
				break
			}
		}
	}
	label := sesskit.SessionStatusLabel(activity.Verdict, activity.Facts.EndedReason)
	status, reason := "at rest", "no live batch work"
	switch {
	case f.Unknown:
		status = "batch unknown (incomplete read)"
		reason = "the batch read was incomplete; live batch work is unknown"
	case f.Live:
		status = "batch live"
		for _, r := range f.Roots {
			if r.Held {
				reason = fmt.Sprintf("batch root %s is %s (held work)", cliout.Short(r.RootID), r.State)
				break
			}
		}
	case lock.Kind == session.LockHeld:
		status = sesskit.AnnotateRecordedEnd("running", activity.Facts.EndedReason)
		reason = "the session lock is held by a live process"
	case label != "":
		status = label
		reason = activity.Verdict.Description
	default:
		reason = activity.Verdict.Description
		// A finished (end_turn) assistant turn is deliberately NOT a
		// session-end signal (classifier decision 4), so the verdict stays
		// idle -- but the reason must explain the contradiction instead of
		// a bare "no recorded end" sentence (FIX-STATUS (b)).
		if lastFinish == string(message.FinishReasonEndTurn) {
			reason += "; the last assistant turn finished (end_turn) but no ended_reason is recorded: the run's exit write may have been dropped under database contention (see the rush log) or the loop was killed after the turn"
		}
	}
	if !f.Unknown && status != "at rest" && activity.Verdict.Kind != session.ActivityCrashed &&
		(activity.Verdict.CrashedPID > 0 || lock.Kind == session.LockDead) {
		status += " (stale lock)"
		if f.Live {
			// Live work outranks the dead recorded holder; say which PID the
			// stale annotation refers to.
			pid := activity.Verdict.CrashedPID
			if pid == 0 {
				pid = int64(lock.PID)
			}
			if pid > 0 {
				reason += fmt.Sprintf(" (the session lock records dead PID %d)", pid)
			}
		}
	}
	printStallHeader(ctx, a, dataDir, sessionID, status, lock, out)
	fmt.Fprintf(out, "reason: %s.\n", reason)
	renderBatchQuestionNote(f, out)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Last assistant message:")
	if msgsErr != nil {
		fmt.Fprintf(out, "  (could not read: %v)\n", msgsErr)
	} else {
		var last *message.Message
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == message.Assistant {
				last = &msgs[i]
				break
			}
		}
		if last == nil {
			fmt.Fprintln(out, "  (none)")
		} else {
			finish := last.FinishPart()
			fmt.Fprintf(out, "  finish_reason: %s\n", finishReasonOrUnknown(finish))
			// The legacy render surfaced a stored error finish's text so the
			// operator sees the cause; the batch render keeps that contract.
			if finish != nil && finish.Reason == message.FinishReasonError {
				errText := finish.Message
				if errText == "" {
					errText = "(error finish reason but no error text stored)"
				}
				fmt.Fprintf(out, "  error:         %s\n", errText)
			}
		}
	}
	if activity.Facts.Driver != nil {
		fmt.Fprintf(out, "driver: %s.\n", describeRunDriver(*activity.Facts.Driver, activity.Facts.DriverOwes))
	}
	// The facts are already read above; render them without a re-read.
	renderBatchRootsForWhy(f, out)
	return nil
}

// describeRunDriver renders the "why" clause naming the live `rush run` loop
// that drives a session, e.g.
//
//	"a `rush run` loop (PID 1234, host 8a3f0c2b alive) drives this session; a reaction is owed and it retries at its next opportunity"
func describeRunDriver(d sessionhosts.SessionDriver, reactionOwed bool) string {
	clause := fmt.Sprintf("a `rush run` loop (PID %d, host %s %s) drives this session", d.PID, cliout.Short(d.HostID), strings.ToLower(d.Status.String()))
	if reactionOwed {
		return clause + "; a reaction is owed and it retries at its next opportunity"
	}
	return clause + "; it waits for its scope to close"
}

// finishReasonOrUnknown returns the finish reason string, or "(unknown)"
// when there is no Finish part at all (message never finished).
func finishReasonOrUnknown(f *message.Finish) string {
	if f == nil || f.Reason == "" {
		return "(unknown)"
	}
	return string(f.Reason)
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

// RenderSessionsJobsBatch is the `sessions jobs` path: batch roots of
// the session and its delegation tree instead of async_jobs rows.
func RenderSessionsJobsBatch(ctx context.Context, a *app.App, dataDir, sessionID string, asJSON bool) error {
	facts := batchRuntimeFacts(ctx, a, dataDir, []string{sessionID})
	f := facts[sessionID]
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		if len(f.Roots) == 0 {
			if f.Unknown {
				return enc.Encode(jobsJSONItem{Kind: "batch", State: "unknown", BatchUnknown: true, BatchAwaitingAnswer: f.HasQuestion})
			}
			return nil
		}
		for _, r := range f.Roots {
			item := jobsJSONItem{
				OwnerSessionID: r.Owner,
				ToolCallID:     r.ToolCallID,
				Kind:           "batch",
				State:          r.State,
				Delivery:       "none",
			}
			if r.Unknown {
				item.State = "unknown"
				item.BatchUnknown = true
			}
			item.BatchUnknown = item.BatchUnknown || f.Unknown
			if r.Held {
				item.Liveness = "held"
			}
			if r.Cancelling {
				item.Liveness = "held"
				item.Hint = "cancelling: stopped but awaiting termination"
			}
			item.BatchRootID = r.RootID
			item.BatchHeld = r.Held
			item.BatchCancelling = r.Cancelling
			item.BatchAwaitingTermination = r.AwaitingTermination
			// Per-root bound fact ONLY: the session-wide HasQuestion fallback is
			// not attributable to this root; the session-level question surface
			// is the summary line and the fallback paths.
			item.BatchAwaitingAnswer = r.AwaitingAnswer
			item.BatchDebt = r.Debt
			item.BatchTasksPending = r.Tasks.Pending
			item.BatchTasksActive = r.Tasks.Active
			item.BatchTasksDone = r.Tasks.Completed + r.Tasks.Failed + r.Tasks.Cancelled + r.Tasks.TimedOut + r.Tasks.Interrupted + r.Tasks.Skipped
			if err := enc.Encode(item); err != nil {
				return err
			}
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tROOT\tTOOL_CALL_ID\tSTATE\tHELD\tAWAITING\tDEBT\tTASKS(p/a/d)")
	for _, r := range f.Roots {
		state := r.State
		if r.Unknown {
			state = "unknown"
		}
		awaiting := "-"
		switch {
		case r.AwaitingTermination:
			awaiting = "termination"
		case r.AwaitingAnswer:
			// Per-root only: a question bound to ANOTHER root's tool call must
			// not render this row as awaiting; the session-level fallback is
			// carried by the summary/note surfaces.
			awaiting = "answer"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%s\t%d\t%d/%d/%d\n",
			cliout.Short(coresession.HashID(r.Owner)), cliout.Short(r.RootID), r.ToolCallID, state,
			r.Held, awaiting, r.Debt, r.Tasks.Pending, r.Tasks.Active,
			r.Tasks.Completed+r.Tasks.Failed+r.Tasks.Cancelled+r.Tasks.TimedOut+r.Tasks.Interrupted+r.Tasks.Skipped)
	}
	w.Flush()
	if len(f.Roots) == 0 {
		if f.Unknown {
			fmt.Println("(batch read incomplete; live batch work unknown)")
		} else {
			// F8: the same empty-state line the legacy path prints (user-facing
			// continuity across the flag; no batch-specific wording).
			fmt.Println("(no jobs)")
		}
	}
	if f.Unknown {
		fmt.Fprintln(os.Stderr, "warning: the batch read was incomplete; some batch roots may be missing")
	}
	return nil
}
