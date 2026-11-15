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
