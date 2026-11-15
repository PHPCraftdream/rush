package apiclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// BatchNode is one node of a batch tree as the API reports it.
type BatchNode struct {
	ID             string `json:"id"`
	ParentID       string `json:"parent_id,omitempty"`
	Kind           string `json:"kind"`
	Ordinal        int64  `json:"ordinal"`
	State          string `json:"state"`
	Cause          string `json:"cause,omitempty"`
	Tool           string `json:"tool,omitempty"`
	OutcomeState   string `json:"outcome_state,omitempty"`
	OutcomeCode    string `json:"outcome_code,omitempty"`
	OutcomeSummary string `json:"outcome_summary,omitempty"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Output         string `json:"output,omitempty"`
	OutputExpired  bool   `json:"output_expired,omitempty"`
	AwaitingAnswer bool   `json:"awaiting_answer,omitempty"`
	Question       string `json:"question,omitempty"`
}

// Batch is a batch root with all of its nodes.
type Batch struct {
	RootID    string      `json:"root_id"`
	State     string      `json:"state"`
	Done      bool        `json:"done"`
	CreatedAt int64       `json:"created_at"`
	UpdatedAt int64       `json:"updated_at"`
	Nodes     []BatchNode `json:"nodes"`
}

// BatchAccepted describes an accepted batch or task submission.
type BatchAccepted struct {
	RootID   string `json:"root_id"`
	State    string `json:"state"`
	Replayed bool   `json:"replayed,omitempty"`
}

// BatchControlAccepted describes a queued stop request.
type BatchControlAccepted struct {
	RequestID string `json:"request_id"`
	State     string `json:"state"`
}

// TaskRequest is the one-prompt batch submission.
type TaskRequest struct {
	Prompt          string `json:"prompt"`
	ResumeSessionID string `json:"resume_session_id,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
}

// BatchPausedError reports a batch whose node is held on a question; the run
// cannot finish until it is answered.
type BatchPausedError struct{ Batch *Batch }

func (e *BatchPausedError) Error() string { return ErrRunPaused.Error() }
func (e *BatchPausedError) Unwrap() error { return ErrRunPaused }

// BatchFailedError reports a batch that ended in a state other than completed.
type BatchFailedError struct{ Batch *Batch }

func (e *BatchFailedError) Error() string {
	if e == nil || e.Batch == nil {
		return "batch failed"
	}
	return "batch " + e.Batch.State
}

// BatchTerminalSuccess reports whether a terminal root state is a success.
func BatchTerminalSuccess(state string) bool { return state == "completed" }

// HeldQuestions returns the nodes that wait for an answer.
func (b *Batch) HeldQuestions() []BatchNode {
	var held []BatchNode
	for _, node := range b.Nodes {
		if node.AwaitingAnswer {
			held = append(held, node)
		}
	}
	return held
}

func idempotencyKeyOrNew(key string) (string, error) {
	if key != "" {
		return key, nil
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("generate idempotency key")
	}
	return "rushapi-" + hex.EncodeToString(raw[:]), nil
}

// CreateTask submits one prompt as a one-item batch; an empty idempotency key is generated.
func (c *Client) CreateTask(ctx context.Context, request TaskRequest, idempotencyKey string) (BatchAccepted, error) {
	key, err := idempotencyKeyOrNew(idempotencyKey)
	if err != nil {
		return BatchAccepted{}, err
	}
	var accepted BatchAccepted
	if err := c.mutateJSON(ctx, http.MethodPost, "/tasks", request, key, &accepted); err != nil {
		return BatchAccepted{}, err
	}
	return accepted, nil
}

// CreateBatch submits a batch in the external batch schema; spec is sent as is.
func (c *Client) CreateBatch(ctx context.Context, spec json.RawMessage, idempotencyKey string) (BatchAccepted, error) {
	if !json.Valid(spec) {
		return BatchAccepted{}, &UsageError{Err: errors.New("batch spec is not valid JSON")}
	}
	key, err := idempotencyKeyOrNew(idempotencyKey)
	if err != nil {
		return BatchAccepted{}, err
	}
	var accepted BatchAccepted
	if err := c.mutateJSON(ctx, http.MethodPost, "/batches", spec, key, &accepted); err != nil {
		return BatchAccepted{}, err
	}
	return accepted, nil
}

// GetBatch returns a batch root with its nodes.
func (c *Client) GetBatch(ctx context.Context, root string) (*Batch, error) {
	var batch Batch
	if err := c.getJSON(ctx, "/batches/"+url.PathEscape(root), nil, &batch); err != nil {
		return nil, err
	}
	return &batch, nil
}

// GetBatchNode returns one node.
func (c *Client) GetBatchNode(ctx context.Context, root, node string) (*BatchNode, error) {
	var out BatchNode
	if err := c.getJSON(ctx, "/batches/"+url.PathEscape(root)+"/nodes/"+url.PathEscape(node), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StopBatch asks the server to stop a whole root, or one node when node is not empty.
func (c *Client) StopBatch(ctx context.Context, root, node string) (BatchControlAccepted, error) {
	path := "/batches/" + url.PathEscape(root) + "/stop"
	if node != "" {
		path = "/batches/" + url.PathEscape(root) + "/nodes/" + url.PathEscape(node) + "/stop"
	}
	var accepted BatchControlAccepted
	if err := c.mutateJSON(ctx, http.MethodPost, path, struct{}{}, "", &accepted); err != nil {
		return BatchControlAccepted{}, err
	}
	return accepted, nil
}

// AnswerBatchQuestion answers the question a node is held on.
func (c *Client) AnswerBatchQuestion(ctx context.Context, root, node, text string) error {
	return c.mutateJSON(ctx, http.MethodPost, "/batches/"+url.PathEscape(root)+"/nodes/"+url.PathEscape(node)+"/answer", struct {
		Text string `json:"text"`
	}{text}, "", nil)
}

// BatchPollInterval is how often WaitBatch reads the root; the API has no long wait for batches.
var BatchPollInterval = time.Second

// WaitBatch polls until the root is terminal. A root that is not completed
// returns the batch with a *BatchFailedError; a node held on a question returns
// the batch with a *BatchPausedError (exit code 6): the caller answers and waits again.
func (c *Client) WaitBatch(ctx context.Context, root string) (*Batch, error) {
	for {
		batch, err := c.GetBatch(ctx, root)
		if err != nil {
			return nil, err
		}
		if batch.Done {
			if BatchTerminalSuccess(batch.State) {
				return batch, nil
			}
			return batch, &BatchFailedError{Batch: batch}
		}
		if len(batch.HeldQuestions()) > 0 {
			return batch, &BatchPausedError{Batch: batch}
		}
		timer := time.NewTimer(BatchPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("wait for batch: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// Health reports service health.
func (c *Client) Health(ctx context.Context) error {
	var response struct {
		Status string `json:"status"`
	}
	return c.getJSON(ctx, "/health", nil, &response)
}

// Version returns the server's version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	var response struct {
		Version string `json:"version"`
	}
	if err := c.getJSON(ctx, "/version", nil, &response); err != nil {
		return "", err
	}
	return response.Version, nil
}

// Models lists available models.
func (c *Client) Models(ctx context.Context) ([]Model, error) {
	var models []Model
	if err := c.getJSON(ctx, "/models", nil, &models); err != nil {
		return nil, err
	}
	if models == nil {
		models = []Model{}
	}
	return models, nil
}

// OpenAPI returns the OpenAPI document bytes, subject to the configured response-size limit.
func (c *Client) OpenAPI(ctx context.Context) ([]byte, error) {
	_, _, data, err := c.do(ctx, http.MethodGet, "/openapi.json", nil, nil, reqOpts{})
	return data, err
}

// ListPermissions lists pending permissions matching filter.
func (c *Client) ListPermissions(ctx context.Context, filter PermissionFilter) ([]Permission, error) {
	var response struct {
		Items []Permission `json:"items"`
	}
	query := make(url.Values)
	if filter.SessionID != "" {
		query.Set("session_id", filter.SessionID)
	}
	if filter.RunID != "" {
		query.Set("run_id", filter.RunID)
	}
	if filter.State != "" {
		query.Set("state", filter.State)
	}
	if err := c.getJSON(ctx, "/permissions", query, &response); err != nil {
		return nil, err
	}
	if response.Items == nil {
		response.Items = []Permission{}
	}
	return response.Items, nil
}

// AnswerPermission allows or denies a permission request.
func (c *Client) AnswerPermission(ctx context.Context, id string, allow bool) error {
	decision := "deny"
	if allow {
		decision = "allow"
	}
	return c.mutateJSON(ctx, http.MethodPost, "/permissions/"+url.PathEscape(id), struct {
		Decision string `json:"decision"`
	}{decision}, "", nil)
}

// Raw sends a request through the configured transport and path validation.
func (c *Client) Raw(ctx context.Context, method, path string, body []byte) ([]byte, int, error) {
	var payload any
	if body != nil {
		payload = json.RawMessage(body)
	}
	status, _, response, err := c.do(ctx, method, path, nil, payload, reqOpts{})
	return response, status, err
}

func (c *Client) getJSON(ctx context.Context, path string, query any, target any) error {
	_, _, data, err := c.do(ctx, http.MethodGet, path, query, nil, reqOpts{})
	if err != nil {
		return err
	}
	if target == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func (c *Client) mutateJSON(ctx context.Context, method, path string, body any, key string, target any) error {
	_, _, data, err := c.do(ctx, method, path, nil, body, reqOpts{IdempotencyKey: key})
	if err != nil {
		return err
	}
	if target == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func paginationQuery(opts ListOptions) url.Values {
	query := make(url.Values)
	if opts.After != "" {
		query.Set("after", opts.After)
	}
	if opts.Limit > 0 {
		query.Set("limit", strconv.Itoa(opts.Limit))
	}
	return query
}

func collectPages[T any](ctx context.Context, max int, list func(context.Context, string, int) (*Page[T], error)) ([]T, error) {
	if max < 0 {
		return nil, errors.New("maximum item count must not be negative")
	}
	items := make([]T, 0)
	after := ""
	seen := make(map[string]struct{})
	for {
		limit := 200
		if remaining := max - len(items); remaining < limit {
			limit = remaining + 1
		}
		if limit < 1 {
			limit = 1
		}
		page, err := list(ctx, after, limit)
		if err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if len(items) > max {
			return nil, fmt.Errorf("result exceeds maximum of %d items", max)
		}
		if page.NextAfter == nil {
			return items, nil
		}
		if _, duplicate := seen[*page.NextAfter]; duplicate || *page.NextAfter == after {
			return nil, errors.New("pagination cursor repeated")
		}
		seen[*page.NextAfter] = struct{}{}
		after = *page.NextAfter
	}
}

// CreateRun submits a run; an empty idempotency key is generated securely.
func (c *Client) CreateRun(ctx context.Context, sessionID string, request RunRequest, idempotencyKey string) (RunAccepted, error) {
	if idempotencyKey == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return RunAccepted{}, errors.New("generate idempotency key")
		}
		idempotencyKey = "rushapi-" + hex.EncodeToString(raw[:])
	}
	var accepted RunAccepted
	if err := c.mutateJSON(ctx, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/runs", request, idempotencyKey, &accepted); err != nil {
		return RunAccepted{}, err
	}
	return accepted, nil
}

// GetRun returns a run and optionally waits for a state change.
func (c *Client) GetRun(ctx context.Context, id string, waitSeconds int) (*Run, error) {
	query := url.Values{}
	if waitSeconds != 0 {
		query.Set("wait", strconv.Itoa(waitSeconds))
	}
	var run Run
	if err := c.getJSON(ctx, "/runs/"+url.PathEscape(id), query, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// WaitRun polls using the server's long-wait endpoint until completion or timeout.
func (c *Client) WaitRun(ctx context.Context, id string, overall time.Duration) (*Run, error) {
	if overall <= 0 {
		return nil, &RunTimeoutError{Err: context.DeadlineExceeded}
	}
	waitCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	for {
		run, err := c.GetRun(waitCtx, id, 25)
		if err != nil {
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, &RunTimeoutError{Err: context.DeadlineExceeded}
			}
			return nil, err
		}
		switch run.State {
		case "succeeded":
			return run, nil
		case "failed":
			return run, runFailure(run)
		case "cancelled":
			failure := runFailure(run)
			failure.Code = "cancelled"
			return run, failure
		case "awaiting_answer":
			return run, &RunPausedError{Run: run}
		}
		if err := waitCtx.Err(); err != nil {
			return nil, &RunTimeoutError{Err: err}
		}
	}
}

// CancelRun requests cancellation of a run.
func (c *Client) CancelRun(ctx context.Context, id string) (*Run, error) {
	var run Run
	if err := c.mutateJSON(ctx, http.MethodPost, "/runs/"+url.PathEscape(id)+"/cancel", struct{}{}, "", &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// Inject adds a message to an active run.
func (c *Client) Inject(ctx context.Context, sessionID, message string, interrupt bool) error {
	mode := "merge"
	if interrupt {
		mode = "interrupt"
	}
	return c.mutateJSON(ctx, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/inject", struct {
		Message string `json:"message"`
		Mode    string `json:"mode"`
	}{message, mode}, "", nil)
}

// RunTimeoutError indicates that the overall run wait duration elapsed.
type RunTimeoutError struct{ Err error }

func (e *RunTimeoutError) Error() string { return "run wait timed out" }
func (e *RunTimeoutError) Unwrap() error { return e.Err }

// RunPausedError carries the run that requires a permission answer.
type RunPausedError struct{ Run *Run }

func (e *RunPausedError) Error() string { return ErrRunPaused.Error() }
func (e *RunPausedError) Unwrap() error { return ErrRunPaused }

func runFailure(run *Run) *RunError {
	if run.Error == nil {
		return &RunError{Run: run}
	}
	failure := *run.Error
	failure.Run = run
	return &failure
}

// SessionsOptions selects a page of sessions.
type SessionsOptions struct {
	After string
	Limit int
}

// ListSessions returns one cursor page of sessions.
func (c *Client) ListSessions(ctx context.Context, opts SessionsOptions) (*Page[Session], error) {
	var page Page[Session]
	if err := c.getJSON(ctx, "/sessions", paginationQuery(ListOptions(opts)), &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// AllSessions reads all sessions, stopping if max items would be exceeded.
func (c *Client) AllSessions(ctx context.Context, max int) ([]Session, error) {
	return collectPages(ctx, max, func(ctx context.Context, after string, limit int) (*Page[Session], error) {
		return c.ListSessions(ctx, SessionsOptions{After: after, Limit: limit})
	})
}

// GetSession returns one session by ID.
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var item Session
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(id), nil, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// CreateSession creates a session with the supplied title.
func (c *Client) CreateSession(ctx context.Context, title string) (*Session, error) {
	var item Session
	if err := c.mutateJSON(ctx, http.MethodPost, "/sessions", map[string]string{"title": title}, "", &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// ForkSession forks a session with the supplied title.
func (c *Client) ForkSession(ctx context.Context, id, title string) (*Session, error) {
	var item Session
	if err := c.mutateJSON(ctx, http.MethodPost, "/sessions/"+url.PathEscape(id)+"/fork", map[string]string{"title": title}, "", &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// DeleteSession deletes a session.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	_, _, _, err := c.do(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(id), nil, nil, reqOpts{})
	return err
}

// ListMessages returns one cursor page of messages.
func (c *Client) ListMessages(ctx context.Context, sessionID, after string, limit int) (*Page[Message], error) {
	var page Page[Message]
	if err := c.getJSON(ctx, "/sessions/"+url.PathEscape(sessionID)+"/messages", paginationQuery(ListOptions{After: after, Limit: limit}), &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// AllMessages reads all messages for a session, stopping if max items would be exceeded.
func (c *Client) AllMessages(ctx context.Context, sessionID string, max int) ([]Message, error) {
	return collectPages(ctx, max, func(ctx context.Context, after string, limit int) (*Page[Message], error) {
		return c.ListMessages(ctx, sessionID, after, limit)
	})
}
