// The "awaiting answer" read side (#1158): the child_question session
// notices #1157 persists are the durable record that a held delegation's
// child paused on a question. This file is the read-only reader every
// surface that cannot reach the coordinator's in-memory ledger (sessions
// why/list, the web live-work panel) goes through: parse the notice's
// canonical frame once, here, so no consumer re-parses the text.
package session

import (
	"context"
	"strconv"
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
	// Batch-frame identity (ParseBatchChildQuestionNotice only; the legacy
	// parser leaves them zero). NodeID is the batch node that asked, Token
	// its incarnation token, Sequence the root's notice sequence.
	NodeID   string
	Token    uint64
	Sequence uint64
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

// batchQuestionMarker opens the batch question frame the agent writes
// (internal/agent/batch_install_question.go):
//
//	[batch-question node=<id> token=<n> sequence=<n> depth=<n> child=<id>]\n
//	SUB-AGENT QUESTION: <question>\n
//	<guidance line>
const batchQuestionMarker = "[batch-question "

const batchQuestionTextPrefix = "SUB-AGENT QUESTION: "

// ParseBatchChildQuestionNotice is the batch twin of
// ParseChildQuestionNotice: it parses the marker-first frame batch
// questions are published with (BatchPublishQuestion stores them with
// job_tool_call_id NULL, so the legacy session-parentheses frame never
// appears). Question rule: the question ends at the FIRST newline after the
// "SUB-AGENT QUESTION: " prefix -- the producer's guidance sentence follows
// after exactly one '\n' (see the comment at the cut below); the same
// display cap via OneLineQuestion applies. A notice without a parsable batch
// frame is skipped, never guessed at.
func ParseBatchChildQuestionNotice(text string) (ChildQuestion, bool) {
	i := strings.Index(text, batchQuestionMarker)
	if i < 0 {
		return ChildQuestion{}, false
	}
	end := strings.Index(text[i:], "]\n")
	if end < 0 {
		return ChildQuestion{}, false
	}
	var q ChildQuestion
	for _, field := range strings.Fields(text[i+len(batchQuestionMarker) : i+end]) {
		key, value, ok := strings.Cut(field, "=")
		if !ok || value == "" {
			return ChildQuestion{}, false
		}
		switch key {
		case "node":
			q.NodeID = value
		case "child":
			q.ChildSessionID = value
		case "token":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return ChildQuestion{}, false
			}
			q.Token = n
		case "sequence":
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return ChildQuestion{}, false
			}
			q.Sequence = n
		}
	}
	if q.NodeID == "" || q.ChildSessionID == "" {
		return ChildQuestion{}, false
	}
	rest := text[i+end+len("]\n"):]
	j := strings.Index(rest, batchQuestionTextPrefix)
	if j < 0 {
		return ChildQuestion{}, false
	}
	question := rest[j+len(batchQuestionTextPrefix):]
	// The producer (internal/agent/batch_install_question.go:36) writes
	// marker + "SUB-AGENT QUESTION: " + question + "\nAnswer with
	// inject_agent ...": ONE newline separates the question from the
	// guidance sentence. The documented rule is therefore: the question ends
	// at the FIRST '\n' after the prefix. A multi-line model question is
	// truncated at its first line by this format -- that is the rule, not a
	// bug (session cannot import internal/agent, hence the file:line cite).
	if k := strings.Index(question, "\n"); k >= 0 {
		question = question[:k]
	}
	q.Question = OneLineQuestion(question)
	return q, true
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
// no delivery state changes. Unparsable rows are skipped. A notice whose
// bound delegation is no longer running (finished, voided or gone -- the
// pull path's own void rule) is dropped.
func (s *AsyncJobStore) PendingChildQuestions(ctx context.Context, owner string) ([]ChildQuestion, error) {
	rq := s.readQuerier()
	rows, err := rq.ListPendingSessionNoticesForOwner(ctx, owner)
	if err != nil {
		return nil, err
	}
	out := make([]ChildQuestion, 0, len(rows))
	for _, row := range rows {
		if row.Kind != NoticeKindChildQuestion {
			continue
		}
		// The pull path's own void rule (ownHostID is unused for this
		// kind); an unread job fact keeps the notice (fail-open).
		if void, voidErr := s.sessionNoticeVoidCondition(ctx, rq, owner, "", row); voidErr == nil && void {
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
