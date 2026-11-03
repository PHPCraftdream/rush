package session

import (
	"context"
	"strings"
)

// BatchAPIRoot is one root batch of an API service session.
type BatchAPIRoot struct{ Root, State, Ref string }

// BatchAPIRoots lists the root batches an owner session holds, oldest first.
// Ref is the submit reference the root was admitted under; the API encodes its
// Idempotency-Key there, so the listing is the durable idempotency index.
func (s *BatchStore) BatchAPIRoots(ctx context.Context, owner string) ([]BatchAPIRoot, error) {
	rows, err := s.writer.QueryContext(ctx, `SELECT root_id,state,COALESCE(tool_call_id,'') FROM batch_nodes WHERE owner_session_id=? AND node_id=root_id ORDER BY created_at,root_id`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BatchAPIRoot{}
	for rows.Next() {
		var x BatchAPIRoot
		if err = rows.Scan(&x.Root, &x.State, &x.Ref); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// BatchAPIRootLive reports whether a root state still occupies a slot of the
// token's live-root budget (pending, starting, running and cancelling count).
func BatchAPIRootLive(state string) bool {
	switch state {
	case "pending", "starting", "running", "cancelling":
		return true
	}
	return false
}

// BatchAPIHeldQuestion is one unanswered child question of an owner's batch.
type BatchAPIHeldQuestion struct{ Node, Child, Question string }

// BatchAPIHeldQuestions lists the owner's unanswered child questions, oldest
// first: the unreacted wake=1 child_question notices. The question text and the
// node/child identity come from the notice marker the publisher wrote.
func (s *BatchStore) BatchAPIHeldQuestions(ctx context.Context, owner string) ([]BatchAPIHeldQuestion, error) {
	rows, err := s.writer.QueryContext(ctx,
		`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery IN ('pending','done') AND wake = 1 AND reacted = 0 ORDER BY id`,
		owner, NoticeKindChildQuestion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BatchAPIHeldQuestion
	for rows.Next() {
		var text string
		if err = rows.Scan(&text); err != nil {
			return nil, err
		}
		if question, ok := parseBatchAPIQuestion(text); ok {
			out = append(out, question)
		}
	}
	return out, rows.Err()
}

// parseBatchAPIQuestion reads the publisher's text:
// "[batch-question node=N token=T sequence=S depth=D child=C]\nSUB-AGENT QUESTION: Q\nAnswer with ...".
func parseBatchAPIQuestion(text string) (BatchAPIHeldQuestion, bool) {
	head, rest, found := strings.Cut(text, "]\n")
	if !found || !strings.HasPrefix(head, "[batch-question ") {
		return BatchAPIHeldQuestion{}, false
	}
	var question BatchAPIHeldQuestion
	for _, field := range strings.Fields(strings.TrimPrefix(head, "[batch-question ")) {
		if value, ok := strings.CutPrefix(field, "node="); ok {
			question.Node = value
		}
		if value, ok := strings.CutPrefix(field, "child="); ok {
			question.Child = value
		}
	}
	body, _ := strings.CutPrefix(rest, "SUB-AGENT QUESTION: ")
	question.Question, _, _ = strings.Cut(body, "\nAnswer with inject_agent")
	return question, question.Node != "" && question.Child != ""
}
