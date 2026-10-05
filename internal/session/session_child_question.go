// The "awaiting answer" read side (#1158): the child_question session
// notices #1157 persists are the durable record that a held delegation's
// child paused on a question. This file is the read-only reader every
// surface that cannot reach the coordinator's in-memory ledger (sessions
// why/list, the web live-work panel) goes through: parse the notice's
// canonical frame once, here, so no consumer re-parses the text.
package session

import (
	"context"
	"strings"
	"unicode/utf8"
)

// ChildQuestionDisplayMaxLen caps the question text any surface quotes
// (in runes — byte slicing would corrupt multi-byte UTF-8).
const ChildQuestionDisplayMaxLen = 200

// childQuestionFramePrefix is the frame the notice text opens its question
// with (agent's childQuestionNoticeText / subAgentQuestionFrame): the child
// session id follows in parentheses, the question text after ": ".
const childQuestionFramePrefix = "SUB-AGENT QUESTION (session "

// ChildQuestion is one parsed pending child_question notice: the child
// that asked, the held delegation X the notice is bound to (its async_jobs
// tool_call_id, the key the web panel matches rows by) and the question's
// first line, display-capped.
type ChildQuestion struct {
	NoticeID             int64
	ChildSessionID       string
	DelegationToolCallID string
	Question             string
}

// ParseChildQuestionNotice extracts the frame's child id and question from
// a child_question notice body. The frame sits inside a larger preamble
// (supervision wording wraps it), so it is searched, not prefix-matched.
// A notice without a parsable frame is skipped, never guessed at.
func ParseChildQuestionNotice(text, jobToolCallID string) (ChildQuestion, bool) {
	i := strings.Index(text, childQuestionFramePrefix)
	if i < 0 {
		return ChildQuestion{}, false
	}
	rest := text[i+len(childQuestionFramePrefix):]
	j := strings.Index(rest, "): ")
	if j < 0 {
		return ChildQuestion{}, false
	}
	childID := rest[:j]
	if childID == "" {
		return ChildQuestion{}, false
	}
	question := rest[j+len("): "):]
	// The question block ends at the next paragraph; the preamble text
	// that follows is not part of the question.
	if k := strings.Index(question, "\n\n"); k >= 0 {
		question = question[:k]
	}
	question = OneLineQuestion(question)
	return ChildQuestion{
		ChildSessionID:       childID,
		DelegationToolCallID: jobToolCallID,
		Question:             question,
	}, true
}

// OneLineQuestion collapses a question block to one display line, capped at
// ChildQuestionDisplayMaxLen runes.
func OneLineQuestion(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= ChildQuestionDisplayMaxLen {
		return s
	}
	runes := []rune(s)
	return string(runes[:ChildQuestionDisplayMaxLen]) + "…"
}

// PendingChildQuestions reads the owner's still-pending child_question
// notices (the delivered ones are already in the parent's history and no
// longer need surfacing) and parses each into a ChildQuestion. Read-only:
// no delivery state changes. Unparsable rows are skipped.
func (s *AsyncJobStore) PendingChildQuestions(ctx context.Context, owner string) ([]ChildQuestion, error) {
	rows, err := s.q.ListPendingSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]ChildQuestion, 0, len(rows))
	for _, row := range rows {
		if row.Kind != NoticeKindChildQuestion {
			continue
		}
		q, ok := ParseChildQuestionNotice(row.Text, row.JobToolCallID.String)
		if !ok {
			continue
		}
		q.NoticeID = row.ID
		out = append(out, q)
	}
	return out, nil
}
