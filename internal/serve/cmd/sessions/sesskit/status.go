package sesskit

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/session"
)

// endedReasonPeakHours is the ended_reason a provider peak-hours pause
// records when a run stops because the provider is inside its peak-hours
// window. It is a pause, not a completion, so every surface renders it as
// paused rather than done.
const EndedReasonPeakHours = "peak_hours"

// sessionStatusLabel renders the classifier's verdict into the STATUS word
// the sessions surfaces print ("running" / "delegating" / "crashed" / "done"
// / "paused: provider peak hours" / an unknown ended_reason verbatim; "" =
// at rest). recordedEnd is the session row's ended_reason read alongside the
// verdict: while live work keeps the session open the classifier
// deliberately ignores a recorded end (classifier decision 4), and without
// this parameter that recorded fact would be invisible in the status -- the
// exact "sessions list said running while sessions status reported a
// recorded exit" mismatch this file exists to close. Passing "" (the old
// verdict-only shape) keeps the plain word.
func SessionStatusLabel(v session.ActivityVerdict, recordedEnd string) string {
	switch v.Kind {
	case session.ActivityInTurn, session.ActivityBetweenTurns:
		return AnnotateRecordedEnd("running", recordedEnd)
	case session.ActivityDelegating:
		return AnnotateRecordedEnd("delegating", recordedEnd)
	case session.ActivityCrashed:
		return "crashed"
	case session.ActivityEnded:
		reason := v.EndedReason
		if reason == "" {
			reason = recordedEnd
		}
		return EndedStatusLabel(reason)
	}
	return ""
}

// annotateRecordedEnd appends the recorded exit to a live-work status word:
// a session that is live despite a recorded ended_reason is exactly the
// anomaly an operator must see ("running (exit timeout)"), and it is the
// same fact `sessions status` reports as exit_reason.
func AnnotateRecordedEnd(status, recordedEnd string) string {
	if recordedEnd == "" {
		return status
	}
	return status + " (exit " + recordedEnd + ")"
}

// endedStatusLabel renders one recorded ended_reason. The known vocabulary
// maps to "done" (the historical whole-column behavior); the peak-hours
// pause maps to its paused label; anything else -- a reason a newer rush
// wrote that this binary does not know -- renders verbatim, never as
// "done".
func EndedStatusLabel(reason string) string {
	switch reason {
	case "":
		return ""
	case EndedReasonPeakHours:
		return "paused: provider peak hours"
	}
	if KnownEndedReason(reason) {
		return "done"
	}
	return reason
}

// knownEndedReason reports whether reason is in the exit_reason vocabulary
// this codebase writes (message.FinishReason* plus the synthetic reasons the
// run result, the loop exits and the batch fault path record).
func KnownEndedReason(reason string) bool {
	switch reason {
	case "awaiting_answer", "batch_fault", "canceled", "done", "end_turn",
		"error", "max_cost", "max_tokens", "queued", "stalled", "stop",
		"timeout", "tool_use", "unknown":
		return true
	}
	return false
}

type PendingChildQuestionReader interface {
	PendingChildQuestions(context.Context, string) ([]session.ChildQuestion, error)
}

func SessionQuestionReaderForApp(a *app.App) PendingChildQuestionReader {
	if a == nil {
		return nil
	}
	if a.DB == nil {
		return nil
	}
	conn := a.DB()
	if conn == nil {
		return nil
	}
	return session.NewSessionNoticeStore(conn)
}

// ListStatusesWithAwaiting maps the classifier's verdicts to the STATUS
// column, adding the awaiting-answer substate (#1158): each listed root's
// pending child_question notices re-classify its verdict (read-only; a
// failed notice read leaves the plain verdict) and become the
// "(awaiting answer)" annotation plus the JSON wire fields.
func ListStatusesWithAwaiting(ctx context.Context, store PendingChildQuestionReader, activities app.SessionActivityBatchResult, ids []string) (map[string]string, map[string]session.ChildQuestion) {
	questionsByRoot := map[string][]session.ChildQuestion{}
	if store != nil {
		for _, id := range ids {
			if qs, err := store.PendingChildQuestions(ctx, id); err == nil && len(qs) > 0 {
				questionsByRoot[id] = qs
			}
		}
	}
	statusByID := make(map[string]string, len(ids))
	awaitingByID := make(map[string]session.ChildQuestion, len(questionsByRoot))
	for id, act := range activities.ByID {
		v := act.Verdict
		if qs := questionsByRoot[id]; len(qs) > 0 {
			f := act.Facts
			f.ChildQuestions = qs
			v = session.ClassifySessionActivity(f)
			awaitingByID[id] = qs[0]
		}
		st := SessionStatusLabel(v, act.Facts.EndedReason)
		if _, ok := questionsByRoot[id]; ok && st != "" {
			st += " (awaiting answer)"
		}
		statusByID[id] = st
	}
	return statusByID, awaitingByID
}
