// Shared read-side batch facts for the sessions commands. Consumers use these
// facts for conservative status, guard, audit, and render decisions.
package sesskit

import (
	"context"
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

// BatchRootFact is one root incarnation's read-side summary: identity, the
// observed state, the held/termination verdict, and the delivery/debt facts
// from the history snapshot where available.
type BatchRootFact struct {
	Owner, RootID, ToolCallID string
	State                     string
	Held                      bool // pending | starting | running | cancelling
	Cancelling                bool
	AwaitingTermination       bool   // cancelling: stopped but awaiting termination
	AwaitingAnswer            bool   // a held child_question notice is bound to this root
	QuestionChildID           string // parsed from the bound notice's frame, when held
	QuestionText              string
	Debt                      int64
	Tasks                     session.BatchLiveCounts
	Unknown                   bool // this root's own read was incomplete or failed
}

// BatchSessionFacts aggregates one session's root facts. Live or Unknown must
// keep every consumer from reporting idle/at rest (conservative-read rule).
type BatchSessionFacts struct {
	Roots   []BatchRootFact
	Live    bool // at least one held root (or an unclassifiable one)
	Unknown bool // any read in the tree was incomplete or errored
	Debt    int64
	Tasks   session.BatchLiveCounts
	// GuardDebt is the pending-inclusive delivery debt (terminal roots'
	// selected unreacted notices plus retained session_notices rows) that
	// protects the session from gc/compact; Debt stays the display count.
	GuardDebt   bool
	GuardDebtN  int
	HasQuestion bool // a held child_question notice awaits an answer
	// QuestionChildID/QuestionText are the parsed bound notice's frame (the
	// first root that carries one); empty when only the session-level
	// fallback flagged the question.
	QuestionChildID string
	QuestionText    string
}

// AwaitingAnswer reports whether any held batch question (session-level
// fallback or a root's bound held question) awaits an answer.
func (f BatchSessionFacts) AwaitingAnswer() bool {
	if f.HasQuestion {
		return true
	}
	for _, r := range f.Roots {
		if r.AwaitingAnswer {
			return true
		}
	}
	return false
}

// addRoot adds one root's facts to the session aggregate.
func (f *BatchSessionFacts) addRoot(r BatchRootFact) {
	f.Roots = append(f.Roots, r)
	if r.QuestionChildID != "" && f.QuestionChildID == "" {
		f.QuestionChildID = r.QuestionChildID
		f.QuestionText = r.QuestionText
	}
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

// BatchRootHeld reports whether a root state is nonterminal. Cancelling is
// held: the projection stays nonterminal until the confirmed termination
// report, so a cancelling root is "stopped but awaiting termination".
func BatchRootHeld(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// BatchStatusAnnotation is the shared conservative suffix every command
// appends to its status word: "(batch live)" for held batch work, "(batch
// unknown)" when the read was incomplete -- never a silent blank.
func BatchStatusAnnotation(f BatchSessionFacts) string {
	switch {
	case f.Unknown:
		return " (batch unknown)"
	case f.Live:
		return " (batch live)"
	}
	return ""
}

// ReduceBatchSessionFacts reads the given sessions' live batch roots through
// the R10a live reader and the R10c history snapshots (delivery/debt per
// root). A nil reader or source, a read error, or an incomplete read marks
// the session Unknown instead of empty. A non-nil q adds the per-root bound
// question check (session_notices by tool call id); nil only skips that
// check, the conservative verdicts still stand.
func ReduceBatchSessionFacts(ctx context.Context, reader *session.BatchLiveReader, store *session.BatchStore, q *db.Queries, ids []string) map[string]BatchSessionFacts {
	out := make(map[string]BatchSessionFacts, len(ids))
	if reader == nil || reader.Source == nil || store == nil || ctx == nil {
		for _, id := range ids {
			out[id] = BatchSessionFacts{Unknown: true, Live: true}
		}
		return out
	}
	live := reader.BatchLiveForSessions(ctx, ids)
	for _, id := range ids {
		var f BatchSessionFacts
		res := live[id]
		if res.Err != nil || !res.Complete {
			f.Unknown = true
			if len(res.Tree) == 0 {
				// An incomplete read with no roots must never read as idle.
				f.Live = true
			}
		}
		for _, root := range res.Tree {
			rf := BatchRootFact{
				Owner:      root.Ref.OwnerSessionID,
				RootID:     root.Ref.RootID,
				ToolCallID: root.ToolCallID,
				State:      root.State,
				Tasks:      root.Counts,
			}
			rf.Held = BatchRootHeld(rf.State)
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
				if snap.Disappeared {
					// Deletion is not a confirmed termination fact: still live.
					rf.State = "disappeared"
					rf.Held = true
				}
			}
			// Bound held question: the R10c query names the notices bound to
			// THIS root's tool call, so a question fact stays per-root.
			if q != nil {
				notices, nerr := q.BatchListBoundNoticesByToolCallID(ctx, db.BatchListBoundNoticesByToolCallIDParams{
					Owner: rf.Owner, ToolCallID: sql.NullString{String: rf.ToolCallID, Valid: true},
				})
				if nerr != nil {
					f.Unknown = true
					f.Live = true
				} else {
					for _, n := range notices {
						if n.Kind == session.NoticeKindChildQuestion && n.Wake == 1 && n.Reacted == 0 &&
							(n.Delivery == "pending" || n.Delivery == "done") {
							rf.AwaitingAnswer = true
							f.HasQuestion = true
							// The notice text embeds the canonical question frame;
							// parse it once here so every surface can quote it.
							if question, ok := session.ParseChildQuestionNotice(n.Text, rf.ToolCallID); ok {
								rf.QuestionChildID = question.ChildSessionID
								rf.QuestionText = question.Question
							}
						}
					}
				}
			}
			f.addRoot(rf)
		}
		// Pending-inclusive delivery debt (terminal roots included) and held
		// questions guard the session from gc/compact even at rest. A debt
		// read error is conservative unknown; skipped on an incomplete live
		// read, which is already unknown (the guard reasons on that first).
		if res.Err == nil && res.Complete {
			debt, debtErr := store.BatchReactionPendingInclusiveDebtRows(ctx, id)
			if debtErr != nil {
				f.Unknown = true
				f.Live = true
			} else {
				f.GuardDebtN = len(debt.Nodes) + len(debt.Notices)
				f.GuardDebt = f.GuardDebtN > 0
				// Conservative catch-all for questions whose bound root is not
				// visible in the live tree: a debt row of kind child_question
				// still flags the session-level question fact (the per-root
				// bound check above is the precise signal; this is the fallback).
				for _, n := range debt.Notices {
					if n.Kind == session.NoticeKindChildQuestion {
						f.HasQuestion = true
					}
				}
				// Batch-published questions are unbound (job_tool_call_id NULL),
				// so the per-root query above never sees them: quote the asking
				// child from the pending notice's batch frame.
				if f.HasQuestion && f.QuestionChildID == "" && q != nil {
					FillBatchQuestionFromPending(ctx, q, id, &f)
				}
			}
		}
		out[id] = f
	}
	return out
}

// UnavailableBatchFacts marks every requested id with explicit unknown facts
// (Unknown + Live): a configless App's batch facts are UNAVAILABLE — a
// conservative unknown — never "no live work". Consumers must refuse to
// report idle.
func UnavailableBatchFacts(ids []string) map[string]BatchSessionFacts {
	out := make(map[string]BatchSessionFacts, len(ids))
	for _, id := range ids {
		out[id] = BatchSessionFacts{Unknown: true, Live: true}
	}
	return out
}

// BatchRuntimeFacts is the production entry point. A nil App or a nil/absent
// database handle → explicit unknown facts from UnavailableBatchFacts — never
// nil, never a panic, and never an "at rest" verdict on unreadable evidence.
// Otherwise the live batch facts of the requested sessions.
func BatchRuntimeFacts(ctx context.Context, a *app.App, dataDir string, ids []string) map[string]BatchSessionFacts {
	if a == nil || a.DB == nil {
		return UnavailableBatchFacts(ids)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn := a.DB()
	if conn == nil {
		return UnavailableBatchFacts(ids)
	}
	reader := session.NewBatchLiveReader(db.New(conn), dataDir)
	return ReduceBatchSessionFacts(ctx, reader, session.NewBatchStore(conn), db.New(conn), ids)
}

// FillBatchQuestionFromPending parses the owner's pending unbound
// child_question notices with the batch frame parser and records the first
// parsable one on the session-level facts. A read error or an unparsable
// frame leaves the generic note, never a guess.
func FillBatchQuestionFromPending(ctx context.Context, q *db.Queries, owner string, f *BatchSessionFacts) {
	rows, err := q.ListPendingSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return
	}
	for _, row := range rows {
		if row.Kind != session.NoticeKindChildQuestion || row.JobToolCallID.Valid {
			continue
		}
		if cq, ok := session.ParseBatchChildQuestionNotice(row.Text); ok {
			f.QuestionChildID = cq.ChildSessionID
			f.QuestionText = cq.Question
			return
		}
	}
}
