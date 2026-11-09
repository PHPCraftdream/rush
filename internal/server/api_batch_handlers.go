package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
)

const (
	apiBatchRetryAfter        = 5 * time.Second
	apiBatchTrustedAgentDepth = 2
)

// apiBatchNode is the explicit DTO of one batch node: never a raw row.
type apiBatchNode struct {
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
	// AwaitingAnswer is set while the node's child holds an unanswered question.
	AwaitingAnswer bool   `json:"awaiting_answer,omitempty"`
	Question       string `json:"question,omitempty"`
}

type apiBatch struct {
	RootID    string         `json:"root_id"`
	State     string         `json:"state"`
	Done      bool           `json:"done"`
	CreatedAt int64          `json:"created_at"`
	UpdatedAt int64          `json:"updated_at"`
	Nodes     []apiBatchNode `json:"nodes"`
}

type apiBatchAccepted struct {
	RootID   string `json:"root_id"`
	State    string `json:"state"`
	Replayed bool   `json:"replayed,omitempty"`
}

type apiTaskRequest struct {
	Prompt          string `json:"prompt"`
	ResumeSessionID string `json:"resume_session_id,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
	MaxAgentDepth   *int   `json:"max_agent_depth,omitempty"`
}

func batchStateDone(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "timed_out", "interrupted", "skipped":
		return true
	}
	return false
}

// batchToolName reads the tool name out of a leaf payload without exposing the payload.
func batchToolName(payload string) string {
	var leaf struct {
		Tool string `json:"tool"`
	}
	if json.Unmarshal([]byte(payload), &leaf) != nil {
		return ""
	}
	return leaf.Tool
}

func apiBatchDTO(view apiBatchView) apiBatch {
	out := apiBatch{RootID: view.Root.NodeID, State: view.Root.State, Done: batchStateDone(view.Root.State), CreatedAt: view.Root.CreatedAt, UpdatedAt: view.Root.UpdatedAt, Nodes: make([]apiBatchNode, 0, len(view.Nodes))}
	for _, node := range view.Nodes {
		out.Nodes = append(out.Nodes, apiBatchNodeDTO(node, view))
	}
	return out
}

func apiBatchNodeDTO(node db.BatchNode, view apiBatchView) apiBatchNode {
	dto := apiBatchNode{ID: node.NodeID, Kind: node.Kind, Ordinal: node.Ordinal, State: node.State, Cause: node.Cause, OutcomeState: node.OutcomeState, OutcomeCode: node.OutcomeCode, OutcomeSummary: node.OutcomeSummary}
	if node.ParentID.Valid {
		dto.ParentID = node.ParentID.String
	}
	if node.ChildSessionID.Valid {
		dto.ChildSessionID = node.ChildSessionID.String
	}
	if question, held := view.Held[node.NodeID]; held && !batchStateDone(node.State) {
		dto.AwaitingAnswer, dto.Question = true, question
	}
	if node.Kind == "task" {
		if node.TaskPayload.Valid {
			dto.Tool = batchToolName(node.TaskPayload.String)
		}
		if output, ok := view.Outputs[node.NodeID]; ok {
			dto.Output = output
		} else if batchStateDone(node.State) {
			// Output is live-process, best effort: say so instead of omitting silently.
			dto.OutputExpired = true
		}
	}
	return dto
}

func apiBatchFailure(w http.ResponseWriter, r *http.Request, err error) {
	var invalid *batchInvalidError
	switch {
	case errors.As(err, &invalid):
		WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", invalid.Error(), "", nil)
	case errors.Is(err, errBatchKeyConflict):
		WriteAPIProblem(w, r, http.StatusUnprocessableEntity, "Unprocessable Entity", "idempotency key was used with a different request body", "", nil)
	case errors.Is(err, errBatchOverloaded):
		retryResponseProblem(w, r)
	case errors.Is(err, errBatchNotFound):
		apiNotFound(w, r, "batch")
	case errors.Is(err, errBatchNoQuestion):
		WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "the node has no current question to answer", "", nil)
	case errors.Is(err, errBatchUnavailable):
		WriteAPIProblem(w, r, http.StatusServiceUnavailable, "Service Unavailable", "batch service unavailable", "", err)
	default:
		apiReadFailure(w, r, err)
	}
}

func retryResponseProblem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "5")
	WriteAPIProblem(w, r, http.StatusTooManyRequests, "Too Many Requests", "batch intake is at capacity; retry later", "", nil)
}

func apiBatchBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, apiRunMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > apiRunMaxBody {
		return nil, &http.MaxBytesError{Limit: apiRunMaxBody}
	}
	return body, nil
}

func apiBatchIdempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key != "" && !validIdempotencyKey(key) {
		WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid Idempotency-Key", "", nil)
		return "", false
	}
	return key, true
}

func apiBatchSubmitResponse(w http.ResponseWriter, r *http.Request, result apiBatchSubmitResult) {
	w.Header().Set("Location", apiBasePath+"/batches/"+result.Root)
	WriteAPIResponse(w, r, http.StatusAccepted, apiBatchAccepted{RootID: result.Root, State: "pending", Replayed: result.Replay})
}

func (s *apiBatchService) createBatchHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := PrincipalFrom(r.Context())
		key, ok := apiBatchIdempotencyKey(w, r)
		if !ok {
			return
		}
		body, err := apiBatchBody(r)
		if err != nil {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid request body", "", err)
			return
		}
		spec, err := agent.ParseBatchExternalSpecJSONWithAgentDepth(body, apiBatchTrustedAgentDepth)
		if err != nil || spec.Batch == nil {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "request body must be a batch within the trusted depth cap", "", err)
			return
		}
		depth := apiBatchTrustedAgentDepth
		var external agent.BatchExternalSpec
		if err := json.Unmarshal(body, &external); err != nil {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid request body", "", err)
			return
		}
		if external.MaxAgentDepth != nil {
			depth = *external.MaxAgentDepth
		}
		ctx, depthErr := agent.WithAgentDepth(r.Context(), depth)
		if depthErr != nil {
			apiReadFailure(w, r, depthErr)
			return
		}
		result, err := s.submit(ctx, principal, spec, key, string(body))
		if err != nil {
			apiBatchFailure(w, r, err)
			return
		}
		apiBatchSubmitResponse(w, r, result)
	}
}

// createTaskHandler is the one-prompt convenience: one `agent` item in a batch.
func (s *apiBatchService) createTaskHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := PrincipalFrom(r.Context())
		key, ok := apiBatchIdempotencyKey(w, r)
		if !ok {
			return
		}
		var payload apiTaskRequest
		raw, err := apiBatchBody(r)
		if err == nil {
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&payload)
			if err == nil && decoder.Decode(new(any)) != io.EOF {
				err = errors.New("trailing JSON value")
			}
		}
		if err != nil || strings.TrimSpace(payload.Prompt) == "" || len(payload.Prompt) > apiRunMaxFieldBytes || payload.TimeoutSeconds < 0 || (payload.MaxAgentDepth != nil && (*payload.MaxAgentDepth < 0 || *payload.MaxAgentDepth > apiBatchTrustedAgentDepth)) {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid request body", "", err)
			return
		}
		input := map[string]string{"prompt": payload.Prompt}
		if payload.ResumeSessionID != "" {
			input["resume_session_id"] = payload.ResumeSessionID
		}
		inputJSON, err := json.Marshal(input)
		if err != nil {
			apiReadFailure(w, r, err)
			return
		}
		depth := apiBatchTrustedAgentDepth
		if payload.MaxAgentDepth != nil {
			depth = *payload.MaxAgentDepth
		}
		leaf, err := json.Marshal(agent.BatchCLITask{Tool: agentToolName, Input: inputJSON, AgentDepth: &depth})
		if err != nil {
			apiReadFailure(w, r, err)
			return
		}
		task := &jobbatch.TaskSpec{Payload: leaf, Timeout: time.Duration(payload.TimeoutSeconds) * time.Second}
		spec := jobbatch.Spec{Batch: &jobbatch.BatchSpec{Mode: jobbatch.ModeSequential, OnFail: jobbatch.OnFailStop, Notify: jobbatch.NotifyAll, Items: []jobbatch.Spec{{Task: task}}}}
		ctx, depthErr := agent.WithAgentDepth(r.Context(), depth)
		if depthErr != nil {
			apiReadFailure(w, r, depthErr)
			return
		}
		result, err := s.submit(ctx, principal, spec, key, string(raw))
		if err != nil {
			apiBatchFailure(w, r, err)
			return
		}
		apiBatchSubmitResponse(w, r, result)
	}
}

func (s *apiBatchService) getBatchHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := PrincipalFrom(r.Context())
		view, err := s.read(r.Context(), principal.TokenID, r.PathValue("root"))
		if err != nil {
			apiBatchFailure(w, r, err)
			return
		}
		WriteAPIResponse(w, r, http.StatusOK, apiBatchDTO(view))
	}
}

func (s *apiBatchService) getNodeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := PrincipalFrom(r.Context())
		view, err := s.read(r.Context(), principal.TokenID, r.PathValue("root"))
		if err != nil {
			apiBatchFailure(w, r, err)
			return
		}
		for _, node := range view.Nodes {
			if node.NodeID == r.PathValue("node") {
				WriteAPIResponse(w, r, http.StatusOK, apiBatchNodeDTO(node, view))
				return
			}
		}
		apiNotFound(w, r, "node")
	}
}

func (s *apiBatchService) stopHandler(node bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := PrincipalFrom(r.Context())
		target := ""
		if node {
			target = r.PathValue("node")
		}
		request, err := s.stopRequest(r.Context(), principal.TokenID, r.PathValue("root"), target)
		if err != nil {
			switch {
			case errors.Is(err, session.ErrBatchControlStale), errors.Is(err, session.ErrBatchControlUnknownHost):
				WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "the batch is no longer running in a live process", "", err)
			default:
				apiBatchFailure(w, r, err)
			}
			return
		}
		WriteAPIResponse(w, r, http.StatusAccepted, struct {
			RequestID string `json:"request_id"`
			State     string `json:"state"`
		}{request.RequestID, "queued"})
	}
}

type apiBatchAnswerRequest struct {
	Text string `json:"text"`
}

func (s *apiBatchService) answerHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, _ := PrincipalFrom(r.Context())
		var payload apiBatchAnswerRequest
		if err := apiDecodeStrictBody(r, &payload); err != nil || strings.TrimSpace(payload.Text) == "" || len(payload.Text) > apiRunMaxFieldBytes {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid request body", "", err)
			return
		}
		if err := s.answerHeld(r.Context(), principal.TokenID, r.PathValue("root"), r.PathValue("node"), payload.Text); err != nil {
			apiBatchFailure(w, r, err)
			return
		}
		WriteAPIResponse(w, r, http.StatusAccepted, struct {
			NodeID    string `json:"node_id"`
			Delivered bool   `json:"delivered"`
		}{r.PathValue("node"), true})
	}
}

func apiBatchRouteDeclarations() []APIRoute {
	root := APIParam{Name: "root", In: "path", Required: true, Description: "Batch root ID"}
	node := APIParam{Name: "node", In: "path", Required: true, Description: "Batch node ID"}
	key := APIParam{Name: "Idempotency-Key", In: "header", Description: "Optional request idempotency key; a replay returns the original root"}
	return []APIRoute{
		{Method: http.MethodPost, Pattern: "/batches", Scope: "run", Summary: "Submit batch", OperationID: "createBatch", Params: []APIParam{key}, ResponseDescription: "Batch accepted"},
		{Method: http.MethodPost, Pattern: "/tasks", Scope: "run", Summary: "Submit one agent task", OperationID: "createTask", Params: []APIParam{key}, ResponseDescription: "Task accepted as a one-item batch"},
		{Method: http.MethodGet, Pattern: "/batches/{root}", Scope: "read", Summary: "Get batch", OperationID: "getBatch", Params: []APIParam{root}, ResponseDescription: "Batch state and nodes"},
		{Method: http.MethodGet, Pattern: "/batches/{root}/nodes/{node}", Scope: "read", Summary: "Get batch node", OperationID: "getBatchNode", Params: []APIParam{root, node}, ResponseDescription: "Batch node"},
		{Method: http.MethodPost, Pattern: "/batches/{root}/stop", Scope: "run", Summary: "Stop batch", OperationID: "stopBatch", Params: []APIParam{root}, ResponseDescription: "Stop request queued"},
		{Method: http.MethodPost, Pattern: "/batches/{root}/nodes/{node}/stop", Scope: "run", Summary: "Stop batch node", OperationID: "stopBatchNode", Params: []APIParam{root, node}, ResponseDescription: "Stop request queued"},
		{Method: http.MethodPost, Pattern: "/batches/{root}/nodes/{node}/answer", Scope: "run", Summary: "Answer a held question", OperationID: "answerBatchQuestion", Params: []APIParam{root, node}, ResponseDescription: "Answer delivered to the existing node execution"},
	}
}

func apiBatchRoutePlaceholders() []APIRoute {
	routes := apiBatchRouteDeclarations()
	for i := range routes {
		routes[i].Handler = apiRunPlaceholder
	}
	return routes
}

// apiBatchRoutes wires the declarations to the service handlers.
func (s *apiBatchService) apiBatchRoutes() []APIRoute {
	routes := apiBatchRouteDeclarations()
	handlers := []http.HandlerFunc{s.createBatchHandler(), s.createTaskHandler(), s.getBatchHandler(), s.getNodeHandler(), s.stopHandler(false), s.stopHandler(true), s.answerHandler()}
	for i := range routes {
		routes[i].Handler = handlers[i]
	}
	return routes
}
