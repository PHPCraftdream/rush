// Reaction-independent held batch question routing (production hang fix):
// a child_question notice whose answer was never ACCEPTED must stay
// actionable no matter what the owner's reaction flow did to it
// (BatchReactionMarkReactedWithMessageUpdate →
// MarkSessionNoticesReactedForOwner marks EVERY owner notice reacted as
// soon as the parent produces any message — reacted is not "answered").
// The route set below reuses session_notices + batch_nodes (no second
// ledger): a notice is a route exactly while its marker names the CURRENT
// live incarnation, and the answer sink (coordinator_agent_inject_batch.go)
// closes the notice rows on acceptance, so an answered question never
// routes again and never blocks a later await.
package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/PHPCraftdream/rush/internal/session"
)

// BatchHeldQuestionRoute is one actionable held batch question: a
// child_question notice whose marker names the CURRENT live incarnation
// (batch_nodes row exists, state live, token matches). An answer accepted
// through the sink voids the notice, so the route set is exactly the
// questions still awaiting an answer — independent of `reacted`.
type BatchHeldQuestionRoute struct {
	NoticeID       int64
	ChildSessionID string
	NodeID         string
	Token          int64
	Depth          int
	Sequence       int64
	Question       string // bounded body line after the marker
}

// ErrBatchAwaitHeldQuestion marks a refusal to sleep/await while a held
// child batch question still awaits an answer.
var ErrBatchAwaitHeldQuestion = errors.New("a held child batch question awaits an answer")

// batchQuestionMarkerSequence extracts the sequence field ("sequence=S") of
// a published question marker header; 0 when absent or unparseable (the
// publisher never writes sequence <= 0, so 0 disqualifies the row, like an
// unparseable marker). Same header-parsing style as
// batchQuestionMarkerDepth.
func batchQuestionMarkerSequence(text string) int64 {
	if !strings.HasPrefix(text, "[batch-question ") {
		return 0
	}
	header := strings.TrimPrefix(text, "[batch-question ")
	if idx := strings.Index(header, "]"); idx >= 0 {
		header = header[:idx]
	}
	for _, field := range strings.Fields(header) {
		if name, value, ok := strings.Cut(field, "="); ok && name == "sequence" {
			s, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0
			}
			return s
		}
	}
	return 0
}

// batchQuestionMarkerBody returns the bounded body line after the marker
// header: the text after "]\n" with the "SUB-AGENT QUESTION: " prefix
// stripped, cut at the guidance sentence ("\nAnswer with") and capped at
// 200 characters.
func batchQuestionMarkerBody(text string) string {
	const maxBody = 200
	_, body, ok := strings.Cut(text, "]\n")
	if !ok {
		return ""
	}
	body = strings.TrimPrefix(body, "SUB-AGENT QUESTION: ")
	body, _, _ = strings.Cut(body, "\nAnswer with")
	if len(body) > maxBody {
		body = body[:maxBody]
	}
	return body
}

// batchActionableHeldQuestionRoutes lists the owner's child_question
// notices that still await an ACCEPTED answer: the retained-debt predicate
// (pending OR delivered, wake=1) WITHOUT a reacted predicate — the owner's
// reaction flow marks every notice reacted without answering the child, so
// reacted must never make an undelivered answer invisible (the production
// hang). The close-on-acceptance write in DeliverHeldAnswer (and the
// execution-side resolution) is what keeps this set bounded. Pool
// discipline: the batch writer pool has exactly ONE connection
// (internal/db/connect.go SetMaxOpenConns(1)), so the notice scan is fully
// read and closed BEFORE any per-candidate fence query. Per candidate the
// fence is the ROOT-joined incarnation read (the node's own row plus its
// root's delivery — a retired root owns no live question channel,
// R12-EF F3): a candidate is skipped as PROVEN-dead only when the node is
// not live, the token does not match, or the root delivery is 'void'. A
// fence READ ERROR is never proven absence: it is collected (first wins)
// and returned with the parsed prefix, so consumers treat the whole answer
// as conservative (err non-nil → refuse). The notice-query/scan error
// paths return err directly. A nil writer is conservative: one unknown
// route plus errBatchFactsUnavailable.
func batchActionableHeldQuestionRoutes(ctx context.Context, writer *sql.DB, owner string) ([]BatchHeldQuestionRoute, error) {
	if writer == nil {
		return []BatchHeldQuestionRoute{{}}, errBatchFactsUnavailable
	}
	rows, err := writer.QueryContext(ctx,
		`SELECT id, text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery IN ('pending','done') AND wake = 1 ORDER BY id`,
		owner, session.NoticeKindChildQuestion)
	if err != nil {
		return []BatchHeldQuestionRoute{}, err
	}
	type noticeRow struct {
		id   int64
		text string
	}
	var pending []noticeRow
	defer rows.Close()
	for rows.Next() {
		var r noticeRow
		if err := rows.Scan(&r.id, &r.text); err != nil {
			return []BatchHeldQuestionRoute{}, err
		}
		pending = append(pending, r)
	}
	if err := rows.Err(); err != nil {
		return []BatchHeldQuestionRoute{}, err
	}
	var out []BatchHeldQuestionRoute
	var firstErr error
	for _, r := range pending {
		node, token, ok := parseBatchQuestionMarker(r.text)
		depth := batchQuestionMarkerDepth(r.text)
		seq := batchQuestionMarkerSequence(r.text)
		if !ok || depth <= 0 || seq <= 0 {
			continue
		}
		// Incarnation fence, root-joined like the store's BatchPublishQuestion:
		// the node's own live state + token AND the root's delivery (a
		// retired root owns no live question channel).
		var rowToken int64
		var state string
		var rootDelivery string
		if err := writer.QueryRowContext(ctx,
			`SELECT n.token, n.state, r.delivery FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id WHERE n.node_id = ?1 AND n.owner_session_id = ?2`,
			node, owner).Scan(&rowToken, &state, &rootDelivery); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue // Удалённая incarnation больше не владеет вопросом.
			}
			// Unknown is NOT proven absence: keep scanning, remember the error.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !batchNodeStateLive(state) || rowToken != token || rootDelivery == "void" {
			continue
		}
		out = append(out, BatchHeldQuestionRoute{
			NoticeID:       r.id,
			ChildSessionID: batchQuestionMarkerChild(r.text),
			NodeID:         node,
			Token:          token,
			Depth:          depth,
			Sequence:       seq,
			Question:       batchQuestionMarkerBody(r.text),
		})
	}
	return out, firstErr
}

// batchHeldQuestionAwaitError builds the await refusal for a route set. An
// empty set OR a conservative-unknown first entry (nil-writer shape) reads
// as "the held-question read is unavailable": refuse conservatively and
// point at the answer tool without naming a question we cannot see.
func batchHeldQuestionAwaitError(routes []BatchHeldQuestionRoute) error {
	if len(routes) == 0 || routes[0].NodeID == "" {
		return fmt.Errorf("%w: the held-question read is unavailable; answer any child_question notice via inject_agent with the exact child_session_id from the notice before awaiting tasks", ErrBatchAwaitHeldQuestion)
	}
	lines := make([]string, 0, len(routes)+2)
	for i, route := range routes {
		if i == 3 {
			lines = append(lines, fmt.Sprintf("(+%d more)", len(routes)-3))
			break
		}
		lines = append(lines, fmt.Sprintf(
			"child session %q is held at batch node %s (token %d, depth %d, sequence %d)",
			route.ChildSessionID, route.NodeID, route.Token, route.Depth, route.Sequence))
	}
	lines = append(lines, "Answer before awaiting: inject_agent(child_session_id=\"<exact child id>\", message=\"<answer>\") releases the held ask in the SAME batch execution; do not resume_session_id and do not re-delegate.")
	return fmt.Errorf("%w:\n%s", ErrBatchAwaitHeldQuestion, strings.Join(lines, "\n"))
}

// batchHeldQuestionHintText is the one bounded line a resume-error suffix
// carries (plain text, no leading newline — callers add it).
func batchHeldQuestionHintText(routes []BatchHeldQuestionRoute) string {
	if len(routes) == 0 || routes[0].NodeID == "" {
		return ""
	}
	r := routes[0]
	return fmt.Sprintf(
		"A held batch question still awaits your answer: inject_agent(child_session_id=%q, message=\"<answer>\") releases the held ask at batch node %s (token %d, depth %d, sequence %d); resuming queues a new run behind it.",
		r.ChildSessionID, r.NodeID, r.Token, r.Depth, r.Sequence)
}

// BatchHeldQuestionSource is the optional capability of the bound
// BatchCutoverFacts for the reaction-independent held-question route read.
// It is deliberately NOT part of the BatchCutoverFacts interface: test fakes
// embed that interface with a nil value and must keep compiling.
type BatchHeldQuestionSource interface {
	ActionableHeldQuestions(ctx context.Context, owner string) ([]BatchHeldQuestionRoute, error)
}

// heldQuestionAwaitRefusal is the root AwaitTasks gate: a non-empty route
// set (or an unreadable read) refuses the await before any sleep/timer is
// armed. A coordinator whose facts lack the capability never gates.
func (c *coordinator) heldQuestionAwaitRefusal(ctx context.Context, owner string) error {
	src, ok := coordinatorBatchCutoverFacts(c).(BatchHeldQuestionSource)
	if !ok {
		return nil
	}
	routes, err := src.ActionableHeldQuestions(ctx, owner)
	if err != nil {
		return batchHeldQuestionAwaitError(routes)
	}
	if len(routes) == 0 {
		return nil
	}
	return batchHeldQuestionAwaitError(routes)
}

// batchHeldQuestionResumeRefusal запрещает новый Run при held-вопросе.
// Ошибка чтения не доказывает отсутствие hold; legacy без capability не меняем.
func (c *coordinator) batchHeldQuestionResumeRefusal(ctx context.Context, owner, child string) string {
	src, ok := coordinatorBatchCutoverFacts(c).(BatchHeldQuestionSource)
	if !ok {
		return ""
	}
	routes, err := src.ActionableHeldQuestions(ctx, owner)
	if err != nil {
		return fmt.Sprintf("resume_session_id %q cannot be resumed while held-question state is unavailable; inspect_agent or inject_agent must address the existing child, not create another delegation", child)
	}
	if len(routes) == 0 {
		return ""
	}
	var childRoutes []BatchHeldQuestionRoute
	for _, route := range routes {
		if route.ChildSessionID == child {
			childRoutes = append(childRoutes, route)
		}
	}
	if len(childRoutes) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"resume_session_id %q would queue a NEW run behind this child's HELD batch question; the held ask wakes only on inject_agent, so the queued run could not start. Answer the held question instead: %s",
		child, batchHeldQuestionHintText(childRoutes))
}

// batchHeldQuestionResumeHint is the resume-error suffix for
// runSubAgent's refusal branches: "" when the capability is absent, the
// read fails, or no route exists.
func (c *coordinator) batchHeldQuestionResumeHint(ctx context.Context, owner string) string {
	src, ok := coordinatorBatchCutoverFacts(c).(BatchHeldQuestionSource)
	if !ok {
		return ""
	}
	routes, err := src.ActionableHeldQuestions(ctx, owner)
	if err != nil || len(routes) == 0 {
		return ""
	}
	return "\n" + batchHeldQuestionHintText(routes)
}
