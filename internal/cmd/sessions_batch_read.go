package cmd

// Batch read side of the four session status commands (R12-B part 1): with
// RUSH_BATCH_RUNTIME on, `sessions jobs/why/show/list` read live batch node
// rows through the R10c session read APIs (BatchLiveReader +
// BatchHistorySnapshot) instead of the legacy async_jobs ledger. Flag off,
// every command keeps its legacy path byte-for-byte. Reads are conservative
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
	"text/tabwriter"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
)

// batchRootFact is one root incarnation's read-side summary: identity, the
// observed state, the held/termination verdict, and the delivery/debt facts
// from the history snapshot where available.
type batchRootFact struct {
	Owner, RootID, ToolCallID string
	State                     string
	Held                      bool // pending | starting | running | cancelling
	Cancelling                bool
	AwaitingTermination       bool // cancelling: stopped but awaiting termination
	AwaitingAnswer            bool // undelivered/unreacted notice or outstanding debt
	Debt                      int64
	Tasks                     session.BatchLiveCounts
	Unknown                   bool // this root's own read was incomplete or failed
}

// batchSessionFacts aggregates one session's root facts. Live or Unknown must
// keep every consumer from reporting idle/at rest (conservative-read rule).
type batchSessionFacts struct {
	Roots   []batchRootFact
	Live    bool // at least one held root (or an unclassifiable one)
	Unknown bool // any read in the tree was incomplete or errored
	Debt    int64
	Tasks   session.BatchLiveCounts
}

func (f *batchSessionFacts) addRoot(r batchRootFact) {
	f.Roots = append(f.Roots, r)
	f.Debt += r.Debt
	f.Tasks.Pending += r.Tasks.Pending
	f.Tasks.Starting += r.Tasks.Starting
	f.Tasks.Running += r.Tasks.Running
	f.Tasks.Cancelling += r.Tasks.Cancelling
	f.Tasks.Active += r.Tasks.Active
	f.Tasks.Completed += r.Tasks.Completed
	f.Tasks.Failed += r.Tasks.Failed
	f.Tasks.Cancelled += r.Tasks.Cancelled
	f.Tasks.TimedOut += r.Tasks.TimedOut
	f.Tasks.Interrupted += r.Tasks.Interrupted
	f.Tasks.Skipped += r.Tasks.Skipped
	if r.Held || r.Unknown {
		f.Live = true
	}
	if r.Unknown {
		f.Unknown = true
	}
}

// batchRootHeld reports whether a root state is nonterminal. Cancelling is
// held: the projection stays nonterminal until the confirmed termination
// report, so a cancelling root is "stopped but awaiting termination".
func batchRootHeld(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// batchStatusAnnotation is the shared conservative suffix every command
// appends to its status word: "(batch live)" for held batch work, "(batch
// unknown)" when the read was incomplete -- never a silent blank.
func batchStatusAnnotation(f batchSessionFacts) string {
	switch {
	case f.Live:
		return " (batch live)"
	case f.Unknown:
		return " (batch unknown)"
	}
	return ""
}

// reduceBatchSessionFacts reads the given sessions' live batch roots through
// the R10a live reader and the R10c history snapshots (delivery/debt per
// root). A nil reader or source, a read error, or an incomplete read marks
// the session Unknown instead of empty.
func reduceBatchSessionFacts(ctx context.Context, reader *session.BatchLiveReader, store *session.BatchStore, ids []string) map[string]batchSessionFacts {
	out := make(map[string]batchSessionFacts, len(ids))
	if reader == nil || reader.Source == nil || store == nil || ctx == nil {
		for _, id := range ids {
			out[id] = batchSessionFacts{Unknown: true, Live: true}
		}
		return out
	}
	live := reader.BatchLiveForSessions(ctx, ids)
	for _, id := range ids {
		var f batchSessionFacts
		res := live[id]
		if res.Err != nil || !res.Complete {
			f.Unknown = true
		}
		for _, root := range res.Tree {
			rf := batchRootFact{
				Owner:      root.Ref.OwnerSessionID,
				RootID:     root.Ref.RootID,
				ToolCallID: root.ToolCallID,
				State:      root.State,
				Tasks:      root.Counts,
			}
			rf.Held = batchRootHeld(rf.State)
			rf.Cancelling = rf.State == "cancelling"
			rf.AwaitingTermination = rf.Cancelling
			snap, err := store.BatchHistorySnapshot(ctx, reader, rf.Owner, rf.RootID)
			switch {
			case err != nil:
				f.Unknown = true
				rf.Unknown = true
			default:
				if !snap.Complete {
					f.Unknown = true
					rf.Unknown = true
				}
				rf.Debt = snap.Debt
				rf.AwaitingAnswer = snap.Debt > 0 || snap.Root.Delivery == "pending"
				if snap.Disappeared {
					// Deletion is not a confirmed termination fact: still live.
					rf.State = "disappeared"
					rf.Held = true
				}
			}
			f.addRoot(rf)
		}
		out[id] = f
	}
	return out
}

// batchRuntimeFacts is the flag-gated production entry point: nil when
// RUSH_BATCH_RUNTIME is off (callers must keep the legacy path untouched).
func batchRuntimeFacts(ctx context.Context, a *app.App, dataDir string, ids []string) map[string]batchSessionFacts {
	if !config.BatchRuntimeEnabled() || a == nil || a.DB() == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reader := session.NewBatchLiveReader(db.New(a.DB()), dataDir)
	return reduceBatchSessionFacts(ctx, reader, session.NewBatchStore(a.DB()), ids)
}

// describeBatchRootsForWhy renders the `sessions why` batch section: one line
// per live root (identity, state, held/termination verdict, debt), then the
// conservative unknown line when the read was incomplete. Replaces the legacy
// async-jobs section while RUSH_BATCH_RUNTIME is on.
func describeBatchRootsForWhy(ctx context.Context, a *app.App, dataDir, sessionID string, out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Batch roots:")
	facts := batchRuntimeFacts(ctx, a, dataDir, []string{sessionID})
	if facts == nil {
		fmt.Fprintln(out, "  (batch runtime flag off)")
		return
	}
	f := facts[sessionID]
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
			short(r.RootID), r.ToolCallID, short(session.HashID(r.Owner)), verdict, r.Debt,
			r.Tasks.Pending, r.Tasks.Active, r.Tasks.Completed+r.Tasks.Failed+r.Tasks.Cancelled+r.Tasks.TimedOut+r.Tasks.Interrupted+r.Tasks.Skipped, awaiting)
	}
	if f.Unknown {
		fmt.Fprintln(out, "  note: the batch read was incomplete; treat live batch work as unknown")
	}
}

// renderSessionsJobsBatch is the flag-ON `sessions jobs` path: batch roots of
// the session and its delegation tree instead of async_jobs rows.
func renderSessionsJobsBatch(ctx context.Context, a *app.App, dataDir, sessionID string, asJSON bool) error {
	facts := batchRuntimeFacts(ctx, a, dataDir, []string{sessionID})
	if facts == nil {
		fmt.Println("(batch runtime flag off)")
		return nil
	}
	f := facts[sessionID]
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		if len(f.Roots) == 0 {
			if f.Unknown {
				return enc.Encode(jobsJSONItem{Kind: "batch", State: "unknown", BatchUnknown: true})
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
			awaiting = "answer"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%s\t%d\t%d/%d/%d\n",
			short(session.HashID(r.Owner)), short(r.RootID), r.ToolCallID, state,
			r.Held, awaiting, r.Debt, r.Tasks.Pending, r.Tasks.Active,
			r.Tasks.Completed+r.Tasks.Failed+r.Tasks.Cancelled+r.Tasks.TimedOut+r.Tasks.Interrupted+r.Tasks.Skipped)
	}
	w.Flush()
	if len(f.Roots) == 0 {
		if f.Unknown {
			fmt.Println("(batch read incomplete; live batch work unknown)")
		} else {
			fmt.Println("(no batch work)")
		}
	}
	if f.Unknown {
		fmt.Fprintln(os.Stderr, "warning: the batch read was incomplete; some batch roots may be missing")
	}
	return nil
}
