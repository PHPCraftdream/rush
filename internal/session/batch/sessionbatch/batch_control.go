package sessionbatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	session "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
	sessionnotice "github.com/PHPCraftdream/rush/internal/session/state/sessionnotice"
	"github.com/PHPCraftdream/rush/internal/store/db"
	"github.com/google/uuid"
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
// Token and Sequence identify one question of one incarnation of the node.
type BatchAPIHeldQuestion struct {
	Node, Child, Question string
	Token, Sequence       int64
}

// BatchAPIHeldQuestions lists the owner's CURRENT-incarnation unresolved
// child questions, oldest first: the wake=1 child_question notices of
// pending-or-delivered delivery, regardless of `reacted` — the owner's
// reaction flow marks every notice reacted without answering, so reacted
// must never hide an unanswered question. ANSWERED questions disappear on
// their own: both resolution paths (the sink close and
// BatchResolveChildQuestions) set delivery='void', which the pending/done
// predicate excludes. Per candidate the same incarnation + root fence as
// the route reader applies: the node must be live with a matching token
// and its root must not be durably retired. An unreadable fence read
// returns the parsed prefix plus the error; the display consumer degrades
// to an empty listing for that call (view.Held), while the ANSWER path
// (answerHeld → BatchAnswerHeld) never depended on the listing, so no
// question becomes unanswerable. markAnswered/questionAnswered stay
// unchanged: their in-memory answered ledger remains the answer-side
// filter; this listing shows exactly the current-incarnation unresolved
// questions regardless of reacted.
func (s *BatchStore) BatchAPIHeldQuestions(ctx context.Context, owner string) ([]BatchAPIHeldQuestion, error) {
	rows, err := s.writer.QueryContext(ctx,
		`SELECT text FROM session_notices WHERE owner = ?1 AND kind = ?2 AND delivery IN ('pending','done') AND wake = 1 ORDER BY id`,
		owner, sessionnotice.NoticeKindChildQuestion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type questionRow struct {
		question  BatchAPIHeldQuestion
		node      string
		token     int64
		state     string
		rowToken  int64
		rootDeliv string
	}
	var pending []questionRow
	for rows.Next() {
		var text string
		if err = rows.Scan(&text); err != nil {
			return nil, err
		}
		if question, ok := parseBatchAPIQuestion(text); ok {
			pending = append(pending, questionRow{question: question, node: question.Node, token: question.Token})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []BatchAPIHeldQuestion
	var firstErr error
	for _, r := range pending {
		// Incarnation + root fence per candidate, root-joined like
		// BatchPublishQuestion. Fully drained before these reads: the writer
		// pool has exactly ONE connection.
		if err := s.writer.QueryRowContext(ctx,
			`SELECT n.token, n.state, r.delivery FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id WHERE n.node_id = ?1 AND n.owner_session_id = ?2`,
			r.node, owner).Scan(&r.rowToken, &r.state, &r.rootDeliv); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue // Root/node уже удалён; это не ошибка чтения.
			}
			// Unreadable fence is never proven absence: remember the first
			// error and keep scanning.
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !BatchAPIRootLive(r.state) || r.rootDeliv == "void" || r.rowToken != r.token {
			continue
		}
		out = append(out, r.question)
	}
	return out, firstErr
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
		if value, ok := strings.CutPrefix(field, "token="); ok {
			question.Token, _ = strconv.ParseInt(value, 10, 64)
		}
		if value, ok := strings.CutPrefix(field, "sequence="); ok {
			question.Sequence, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	body, _ := strings.CutPrefix(rest, "SUB-AGENT QUESTION: ")
	question.Question, _, _ = strings.Cut(body, "\nAnswer with inject_agent")
	return question, question.Node != "" && question.Child != ""
}

// BatchOwnsRows reports whether the session owns any batch row, live or
// terminal. Deleting such a session cascades the rows away, so sessions gc keeps
// it until the batch retention purge removes the rows.
func (s *BatchStore) BatchOwnsRows(ctx context.Context, owner string) (bool, error) {
	var one int
	err := s.writer.QueryRowContext(ctx, `SELECT 1 FROM batch_nodes WHERE owner_session_id=? LIMIT 1`, owner).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

var (
	ErrBatchControlStale        = errors.New("batch control: stale incarnation")
	ErrBatchControlHostMismatch = errors.New("batch control: host mismatch")
	ErrBatchControlTerminal     = errors.New("batch control: terminal target")
	ErrBatchControlUnknownHost  = errors.New("batch control: unknown host")
)

type BatchControlTarget struct{ Owner, Host, Root, Node string }

func batchControlTarget(ctx context.Context, q *db.Queries, t BatchControlTarget) (db.BatchNode, error) {
	n, err := q.GetBatchNode(ctx, db.GetBatchNodeParams{OwnerSessionID: t.Owner, RootID: t.Root, NodeID: t.Node})
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrBatchControlStale
	}
	if err != nil {
		return n, err
	}
	if n.HostID != t.Host {
		return n, ErrBatchControlHostMismatch
	}
	r, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{RootID: t.Root, OwnerSessionID: t.Owner})
	if err != nil {
		return n, err
	}
	if !batchStateLive(r.State) {
		return n, ErrBatchControlStale
	}
	if !batchStateLive(n.State) {
		return n, ErrBatchControlTerminal
	}
	return n, nil
}

// BatchRequestStop rejects deleted/wrong-owner/wrong-host targets without
// persistence: the schema's exact-incarnation FK forbids such request rows.
// Unknown liveness writes nothing. Dead hosts use recovery, never cancellation.
func (s *BatchStore) BatchRequestStop(ctx context.Context, t BatchControlTarget, probe BatchHostProbe) (db.BatchControlRequest, error) {
	var empty db.BatchControlRequest
	if _, err := batchControlTarget(ctx, db.New(s.writer), t); err != nil {
		return empty, err
	}
	if probe == nil {
		return empty, ErrBatchControlUnknownHost
	}
	status, err := probe(t.Host)
	if err != nil {
		return empty, errors.Join(ErrBatchControlUnknownHost, err)
	}
	switch status {
	case sessionhosts.HostStatusDead:
		_, err = s.BatchRecoverDeadHost(ctx, t.Host, probe)
		return empty, errors.Join(ErrBatchControlStale, err)
	case sessionhosts.HostStatusAlive:
	default:
		return empty, ErrBatchControlUnknownHost
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return empty, err
	}
	if _, err = batchControlTarget(ctx, q, t); err != nil {
		return empty, err
	}
	// The existing creation API is intentionally not called: it owns a different writer.
	r, err := q.CreateBatchControlRequest(ctx, db.CreateBatchControlRequestParams{RequestID: uuid.NewString(), OwnerSessionID: t.Owner, HostID: t.Host, RootID: t.Root, NodeID: t.Node, CreatedAt: time.Now().Unix()})
	if err != nil {
		return empty, err
	}
	return r, tx.Commit()
}

func (s *BatchStore) BatchReadControl(ctx context.Context, t BatchControlTarget, id string) (db.BatchControlRequest, error) {
	r, err := db.New(s.writer).GetBatchControlRequest(ctx, db.GetBatchControlRequestParams{RequestID: id, OwnerSessionID: t.Owner, HostID: t.Host, RootID: t.Root})
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.NodeID != t.Node) {
		return db.BatchControlRequest{}, ErrBatchControlStale
	}
	return r, err
}

func (s *BatchStore) BatchWaitControl(ctx context.Context, t BatchControlTarget, id string) (db.BatchControlRequest, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		r, err := s.BatchReadControl(ctx, t, id)
		if err != nil || r.Status == "answered" {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-ticker.C:
		}
	}
}

// BatchControlAccepted is proof of an accepted AND committed exact-node Stop.
// Sequence must be positive; Stop intent never synthesizes settlement.
type BatchControlAccepted struct {
	Code     string
	Sequence int64
}

// BatchControlCommit must fence Owner/Host/Root/Node in the owning Runner and
// deduplicate RequestID across pump replacement or failure after callback commit.
// It may project via this store: no writer transaction is held across callback.
// Return only after the accepted transition commits, or an error (no answer).
type BatchControlCommit func(context.Context, db.BatchControlRequest) (BatchControlAccepted, error)

// BatchControlPump is explicitly serial. The host boundary must retain ONE
// pump per owning Runner incarnation; multiple requesters may use other stores.
// Host identity is supplied by that boundary, not by requester input.
type BatchControlPump struct {
	mu     sync.Mutex
	store  *BatchStore
	host   string
	commit BatchControlCommit
}

func NewBatchControlPump(s *BatchStore, owningHost string, commit BatchControlCommit) *BatchControlPump {
	return &BatchControlPump{store: s, host: owningHost, commit: commit}
}

func (p *BatchControlPump) Pump(ctx context.Context, owner, root string) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.host == "" || p.commit == nil {
		return 0, ErrBatchControlHostMismatch
	}
	r, err := db.New(p.store.writer).GetBatchRoot(ctx, db.GetBatchRootParams{RootID: root, OwnerSessionID: owner})
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrBatchControlStale
	}
	if err != nil {
		return 0, err
	}
	if r.HostID != p.host {
		return 0, ErrBatchControlHostMismatch
	}
	requests, err := p.store.PendingControls(ctx, owner, p.host, root)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, request := range requests {
		t := BatchControlTarget{Owner: owner, Host: p.host, Root: root, Node: request.NodeID}
		current, e := p.store.BatchReadControl(ctx, t, request.RequestID)
		if e != nil {
			return count, e
		}
		if current.Status != "pending" {
			continue
		}
		if _, e = batchControlTarget(ctx, db.New(p.store.writer), t); e != nil {
			if errors.Is(e, ErrBatchControlTerminal) || errors.Is(e, ErrBatchControlStale) {
				if e = p.answerClosed(ctx, t, current.RequestID); e != nil {
					return count, e
				}
				count++
				continue
			}
			return count, e
		}
		accepted, e := p.commit(ctx, current)
		if e != nil {
			return count, e
		}
		if accepted.Sequence <= 0 || accepted.Code == "" {
			return count, session.ErrBatchProjectionConflict
		}
		// Exact identity CAS; answered controls are never redelivered by this pump.
		e = p.store.AnswerControl(ctx, db.AnswerBatchControlRequestParams{RequestID: current.RequestID, OwnerSessionID: owner, HostID: p.host, RootID: root, NodeID: current.NodeID, RunnerID: current.RunnerID, AnswerCode: accepted.Code, AnswerSequence: sql.NullInt64{Int64: accepted.Sequence, Valid: true}, AnsweredAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}})
		if e != nil {
			return count, e
		}
		count++
	}
	return count, nil
}

// answerClosed validates and answers terminal races in one writer transaction.
func (p *BatchControlPump) answerClosed(ctx context.Context, t BatchControlTarget, id string) error {
	tx, err := p.store.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := db.New(tx)
	if err = q.BatchRetentionWriter(ctx); err != nil {
		return err
	}
	root, err := q.GetBatchRoot(ctx, db.GetBatchRootParams{RootID: t.Root, OwnerSessionID: t.Owner})
	if err != nil {
		return err
	}
	node, err := q.GetBatchNode(ctx, db.GetBatchNodeParams{RootID: t.Root, NodeID: t.Node, OwnerSessionID: t.Owner})
	if err != nil {
		return err
	}
	if root.HostID != p.host || node.HostID != p.host {
		return ErrBatchControlHostMismatch
	}
	if node.RunnerID != root.RunnerID {
		return session.ErrBatchProjectionConflict
	}
	code := "terminal"
	if !batchStateLive(root.State) {
		code = "stale"
	} else if batchStateLive(node.State) {
		return session.ErrBatchProjectionConflict
	}
	sequence := root.RootSequence
	if node.ProjectionSequence > sequence {
		sequence = node.ProjectionSequence
	}
	if sequence <= 0 {
		return session.ErrBatchProjectionConflict
	}
	err = session.BatchRequireRow(q.AnswerBatchControlRequest(ctx, db.AnswerBatchControlRequestParams{RequestID: id, OwnerSessionID: t.Owner, RootID: t.Root, NodeID: t.Node, HostID: p.host, RunnerID: node.RunnerID, AnswerCode: code, AnswerSequence: sql.NullInt64{Int64: sequence, Valid: true}, AnsweredAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}}))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// BatchPublishQuestion durably publishes one held batch question. The node's
// root, incarnation token and live state are resolved in the serialized
// writer transaction (any node id of the tree is addressable — the agent
// family launches on child nodes like "root.1"), then the exact-header
// idempotency insert runs into the existing session_notices outbox with
// job_tool_call_id NULL so the pull-time void rule retains the row.
func (s *BatchStore) BatchPublishQuestion(ctx context.Context, owner, node string, token int64, marker, text string) (retErr error) {
	if owner == "" || node == "" || token <= 0 || marker == "" || !strings.HasSuffix(marker, "\n") {
		return errors.New("batch store: invalid question identity")
	}
	// Refusals and duplicates are decided before admission: neither is
	// productive work, so neither may mint a finalization revision.
	if err := batchQuestionCheckLive(ctx, s.writer, owner, node, token); err != nil {
		return err
	}
	if duplicate, err := batchQuestionDuplicate(ctx, s.writer, owner, marker); err != nil {
		return fmt.Errorf("batch store: question insert: %w", err)
	} else if duplicate {
		return nil
	}
	release, admission, err := session.AdmitFinalizationMutation(ctx, s.writer, owner)
	if release != nil {
		defer func() { retErr = session.BatchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return session.FinalizationFenceError(err)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("batch store: question begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := session.ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return session.FinalizationFenceError(err)
	}
	if err := batchQuestionCheckLive(ctx, tx, owner, node, token); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO session_notices (owner, kind, text, wake, delivery, job_tool_call_id, created_at, updated_at)
SELECT ?, ?, ?, 1, 'pending', NULL, ?, ?
WHERE NOT EXISTS (SELECT 1 FROM session_notices WHERE owner = ? AND kind = ? AND substr(text, 1, length(?)) = ?)`,
		owner, sessionnotice.NoticeKindChildQuestion, text, time.Now().Unix(), time.Now().Unix(),
		owner, sessionnotice.NoticeKindChildQuestion, marker, marker)
	if err != nil {
		return fmt.Errorf("batch store: question insert: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("batch store: question insert: %w", err)
	}
	if inserted == 0 {
		// Exact identity already durably published: dedup hit, commit the
		// validated read and report success so concurrent publishers agree.
		return tx.Commit()
	}
	if err := session.ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return session.FinalizationFenceError(err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("batch store: question commit: %w", err)
	}
	return nil
}

type batchQuestionRower interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// batchQuestionCheckLive pins the exact live node incarnation.
func batchQuestionCheckLive(ctx context.Context, q batchQuestionRower, owner, node string, token int64) error {
	var rowToken int64
	var state string
	var rootDelivery string
	err := q.QueryRowContext(ctx,
		`SELECT n.token, n.state, r.delivery FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.node_id = ? AND n.owner_session_id = ?`,
		node, owner).Scan(&rowToken, &state, &rootDelivery)
	if err != nil {
		return fmt.Errorf("batch store: question node lookup: %w", err)
	}
	// "starting" is the durable post-admission/pre-EventStarted state
	// (engine admission.go:90); the token fence still pins the exact
	// incarnation, and inject_agent is accepted in this state
	// (runner_requests.go refuses only terminal/cancelling).
	if rowToken != token || (state != "pending" && state != "starting" && state != "running" && state != "cancelling") {
		return errors.New("batch store: question node incarnation is not live")
	}
	// R12-EF fix F3: a root the rerun retired durably (delivery='void') owns
	// no live question channel -- refusing here keeps a still-live child of
	// the retired subtree (stop not yet delivered, or a foreign host) from
	// publishing a question the new branch could never answer.
	if rootDelivery == "void" {
		return errors.New("batch store: question root is durably retired")
	}
	return nil
}

func batchQuestionDuplicate(ctx context.Context, q batchQuestionRower, owner, marker string) (bool, error) {
	var duplicate bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_notices WHERE owner = ? AND kind = ? AND substr(text, 1, length(?)) = ?)`,
		owner, sessionnotice.NoticeKindChildQuestion, marker, marker).Scan(&duplicate)
	return duplicate, err
}

// parseBatchQuestionMarker extracts (nodeID, token) from a child-question
// notice text whose header line is the marker BatchPublishQuestion dedupes
// on: "[batch-question node=X token=N sequence=S depth=D child=C]\n". The
// identical parser on the agent side (LiveHeldQuestions' incarnation fence)
// reads the same publisher format; the duplicate exists because session
// cannot import agent.
func parseBatchQuestionMarker(text string) (nodeID string, token int64, ok bool) {
	if !strings.HasPrefix(text, "[batch-question ") {
		return "", 0, false
	}
	header := strings.TrimPrefix(text, "[batch-question ")
	if idx := strings.Index(header, "]"); idx >= 0 {
		header = header[:idx]
	}
	for _, field := range strings.Fields(header) {
		name, value, isField := strings.Cut(field, "=")
		if !isField {
			continue
		}
		switch name {
		case "node":
			nodeID = value
		case "token":
			t, perr := strconv.ParseInt(value, 10, 64)
			if perr != nil {
				return "", 0, false
			}
			token = t
		}
	}
	if nodeID == "" || token == 0 {
		return "", 0, false
	}
	return nodeID, token, true
}

// parseBatchQuestionMarkerFull extracts (node, token, sequence, child) from
// a published question marker header; ok=false when the header is absent,
// unparseable, or any of the identity fields is zero/empty (the publisher
// never writes those). Mirrors the agent-side parsers; the duplicate
// exists because session cannot import agent.
func parseBatchQuestionMarkerFull(text string) (node string, token, sequence int64, child string, ok bool) {
	if !strings.HasPrefix(text, "[batch-question ") {
		return "", 0, 0, "", false
	}
	header := strings.TrimPrefix(text, "[batch-question ")
	if idx := strings.Index(header, "]"); idx >= 0 {
		header = header[:idx]
	}
	for _, field := range strings.Fields(header) {
		name, value, isField := strings.Cut(field, "=")
		if !isField {
			continue
		}
		switch name {
		case "node":
			node = value
		case "child":
			child = value
		case "token":
			t, perr := strconv.ParseInt(value, 10, 64)
			if perr != nil {
				return "", 0, 0, "", false
			}
			token = t
		case "sequence":
			sq, perr := strconv.ParseInt(value, 10, 64)
			if perr != nil {
				return "", 0, 0, "", false
			}
			sequence = sq
		}
	}
	if node == "" || child == "" || token <= 0 || sequence <= 0 {
		return "", 0, 0, "", false
	}
	return node, token, sequence, child, true
}

// BatchResolveChildQuestions durably closes (delivery='void', wake=0) every
// pending-or-delivered child_question notice of owner whose marker names
// EXACTLY this incarnation (node, token, child) with a sequence in
// [1, sequence]: the notice must never outlive the hold it advertises. Rows
// of a NEWER sequence — a question asked after this resolution began — are
// untouched; rows naming a different child are not ours to close. Returns
// the closed count; an unknown identity is a no-op (0, nil), never an
// error. Runs in one serialized writer transaction like
// BatchPublishQuestion (the writer pool has exactly ONE connection).
func (s *BatchStore) BatchResolveChildQuestions(ctx context.Context, owner, node, child string, token, sequence int64) (closed int64, retErr error) {
	if owner == "" || node == "" || child == "" || token <= 0 || sequence <= 0 {
		return 0, nil
	}
	// Matching is decided before admission: a no-op resolve is not
	// productive work and must not mint a finalization revision.
	matched, err := batchMatchingChildQuestionIDs(ctx, s.writer, owner, node, child, token, sequence)
	if err != nil {
		return 0, err
	}
	if len(matched) == 0 {
		return 0, nil
	}
	release, admission, err := session.AdmitFinalizationMutation(ctx, s.writer, owner)
	if release != nil {
		defer func() { retErr = session.BatchCommitReleaseError(retErr, release()) }()
	}
	if err != nil {
		return 0, session.FinalizationFenceError(err)
	}
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("batch store: question resolve begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := session.ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
		return 0, session.FinalizationFenceError(err)
	}
	stale, err := batchMatchingChildQuestionIDs(ctx, tx, owner, node, child, token, sequence)
	if err != nil {
		return 0, err
	}
	if len(stale) > 0 {
		placeholders := make([]string, len(stale))
		args := make([]any, 0, len(stale)+3)
		args = append(args, time.Now().Unix(), sessionnotice.NoticeKindChildQuestion)
		for i, id := range stale {
			placeholders[i] = fmt.Sprintf("?%d", i+3)
			args = append(args, id)
		}
		res, err := tx.ExecContext(ctx, fmt.Sprintf(
			`UPDATE session_notices SET delivery = 'void', wake = 0, updated_at = ?1 WHERE kind = ?2 AND id IN (%s)`,
			strings.Join(placeholders, ", ")), args...)
		if err != nil {
			return 0, fmt.Errorf("batch store: question resolve update: %w", err)
		}
		if closed, err = res.RowsAffected(); err != nil {
			return 0, fmt.Errorf("batch store: question resolve affected: %w", err)
		}
		if err := session.ValidateBatchFinalizationTx(ctx, tx, []string{owner}, admission); err != nil {
			return 0, session.FinalizationFenceError(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("batch store: question resolve commit: %w", err)
	}
	return closed, nil
}

type batchQuestionQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// batchMatchingChildQuestionIDs lists the owner's pending-or-delivered
// question notices naming exactly this incarnation, fully drained before
// return (the writer pool has exactly ONE connection).
func batchMatchingChildQuestionIDs(ctx context.Context, q batchQuestionQuerier, owner, node, child string, token, sequence int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, text FROM session_notices WHERE owner = ? AND kind = ? AND delivery IN ('pending','done') AND wake = 1 ORDER BY id`,
		owner, sessionnotice.NoticeKindChildQuestion)
	if err != nil {
		return nil, fmt.Errorf("batch store: question resolve list: %w", err)
	}
	defer rows.Close()
	var stale []int64
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, fmt.Errorf("batch store: question resolve scan: %w", err)
		}
		mnode, mtoken, msequence, mchild, ok := parseBatchQuestionMarkerFull(text)
		if !ok || mnode != node || mtoken != token || msequence > sequence || mchild != child {
			continue
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("batch store: question resolve iterate: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("batch store: question resolve close: %w", err)
	}
	return stale, nil
}

// batchRerunVoidChildQuestionsOfVoidRoots voids (delivery='void', wake=0)
// every pending-or-delivered child_question notice of owner whose marker
// names a node of a void-delivery root (R12-EF fix F3): the question channel
// of a subtree the rerun retired must never be delivered by the new branch.
// Runs inside BatchRerunReconcile's transaction, after
// BatchRerunVoidNodesOfVoidedRoots, so roots this very transaction retired
// are matched. Notices without a parseable marker (never written by
// BatchPublishQuestion) and questions of surviving roots are untouched; a
// second pass is a no-op (already-void rows never enter the candidate set).
func batchRerunVoidChildQuestionsOfVoidRoots(ctx context.Context, tx *sql.Tx, owner string, now int64) error {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, text FROM session_notices WHERE owner = ? AND kind = ? AND delivery IN ('pending','done')`,
		owner, sessionnotice.NoticeKindChildQuestion)
	if err != nil {
		return fmt.Errorf("batch rerun: list child questions: %w", err)
	}
	defer rows.Close()
	var stale []int64
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			return fmt.Errorf("batch rerun: scan child question: %w", err)
		}
		node, token, ok := parseBatchQuestionMarker(text)
		if !ok {
			continue
		}
		var one int64
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.node_id = ? AND n.owner_session_id = ? AND n.token = ? AND r.delivery = 'void'`,
			node, owner, token).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("batch rerun: match child question root: %w", err)
		}
		stale = append(stale, id)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("batch rerun: iterate child questions: %w", err)
	}
	for _, id := range stale {
		if _, err := tx.ExecContext(ctx,
			`UPDATE session_notices SET delivery = 'void', wake = 0, updated_at = ? WHERE id = ? AND kind = ?`,
			now, id, sessionnotice.NoticeKindChildQuestion); err != nil {
			return fmt.Errorf("batch rerun: void child question: %w", err)
		}
	}
	return nil
}
