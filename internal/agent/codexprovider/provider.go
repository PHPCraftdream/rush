// Package codexprovider implements direct access to ChatGPT's Codex Responses API.
package codexprovider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/object"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/oauth"
)

const (
	providerID      = "openai-codex"
	responsesURL    = "https://chatgpt.com/backend-api/codex/responses"
	codexVersion    = "0.159.0"
	maxErrorBody    = 1 << 20
	maxSSEEventSize = 4 << 20
)

type provider struct {
	client *http.Client
	token  oauth.Token
}

// New creates a provider that authenticates only with a ChatGPT OAuth token.
func New(client *http.Client, token *oauth.Token) (fantasy.Provider, error) {
	if token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("openai-codex requires an OAuth access token")
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &provider{client: client, token: *token}, nil
}

func (p *provider) Name() string { return providerID }

func (p *provider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	if strings.TrimSpace(modelID) == "" {
		return nil, errors.New("openai-codex model ID is empty")
	}
	return &languageModel{provider: p, modelID: modelID}, nil
}

type languageModel struct {
	provider *provider
	modelID  string
}

func (m *languageModel) Provider() string { return providerID }
func (m *languageModel) Model() string    { return m.modelID }

func (m *languageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return object.GenerateWithTool(ctx, m, call)
}

func (m *languageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return object.StreamWithTool(ctx, m, call)
}

func (m *languageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	stream, err := m.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	var content fantasy.ResponseContent
	var text strings.Builder
	var reasoning strings.Builder
	var tools []fantasy.ToolCallContent
	finishReason := fantasy.FinishReasonUnknown
	var usage fantasy.Usage
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeError:
			return nil, part.Error
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeReasoningDelta:
			reasoning.WriteString(part.Delta)
		case fantasy.StreamPartTypeToolCall:
			tools = append(tools, fantasy.ToolCallContent{
				ToolCallID: part.ID,
				ToolName:   part.ToolCallName,
				Input:      part.ToolCallInput,
			})
		case fantasy.StreamPartTypeFinish:
			finishReason = part.FinishReason
			usage = part.Usage
		}
	}
	if text.Len() > 0 {
		content = append(content, fantasy.TextContent{Text: text.String()})
	}
	if reasoning.Len() > 0 {
		content = append(content, fantasy.ReasoningContent{Text: reasoning.String()})
	}
	for _, tool := range tools {
		content = append(content, tool)
	}
	return &fantasy.Response{Content: content, FinishReason: finishReason, Usage: usage}, nil
}

func (m *languageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	body, err := makeRequestBody(ctx, m.modelID, call)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode Codex request: %w", err)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		m.stream(ctx, payload, call.UserAgent, yield)
	}, nil
}

func makeRequestBody(ctx context.Context, modelID string, call fantasy.Call) (map[string]any, error) {
	input := make([]any, 0, len(call.Prompt))
	for _, message := range call.Prompt {
		role := string(message.Role)
		switch role {
		case "system", "developer":
			role = "developer"
		case "user", "assistant":
		case "tool":
			for _, part := range message.Content {
				toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok {
					return nil, fmt.Errorf("openai-codex cannot encode %T in a tool result", part)
				}
				output, err := encodeToolOutput(toolResult.Output)
				if err != nil {
					return nil, err
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": sanitizeCallID(toolResult.ToolCallID),
					"output":  output,
				})
			}
			continue
		default:
			return nil, fmt.Errorf("openai-codex cannot encode message role %q", message.Role)
		}

		var content []any
		flushContent := func() {
			if len(content) > 0 {
				input = append(input, map[string]any{"type": "message", "role": role, "content": content})
				content = nil
			}
		}
		for _, part := range message.Content {
			if part == nil {
				return nil, errors.New("openai-codex cannot encode a nil message part")
			}
			switch part.GetType() {
			case fantasy.ContentTypeText:
				value, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
				if !ok {
					return nil, fmt.Errorf("openai-codex cannot encode text part %T", part)
				}
				kind := "input_text"
				if role == "assistant" {
					kind = "output_text"
				}
				content = append(content, map[string]any{"type": kind, "text": value.Text})
			case fantasy.ContentTypeFile:
				value, ok := fantasy.AsMessagePart[fantasy.FilePart](part)
				if !ok {
					return nil, fmt.Errorf("openai-codex cannot encode file part %T", part)
				}
				dataURI := "data:" + value.MediaType + ";base64," + base64.StdEncoding.EncodeToString(value.Data)
				if strings.HasPrefix(strings.ToLower(value.MediaType), "image/") {
					content = append(content, map[string]any{"type": "input_image", "image_url": dataURI})
				} else {
					content = append(content, map[string]any{"type": "input_file", "file_data": dataURI, "filename": value.Filename})
				}
			case fantasy.ContentTypeToolCall:
				if role != "assistant" {
					return nil, fmt.Errorf("openai-codex tool call appears in %s message", role)
				}
				value, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
				if !ok {
					return nil, fmt.Errorf("openai-codex cannot encode tool call part %T", part)
				}
				flushContent()
				arguments := value.Input
				if strings.TrimSpace(arguments) == "" {
					arguments = "{}"
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   sanitizeCallID(value.ToolCallID),
					"name":      value.ToolName,
					"arguments": arguments,
				})
			case fantasy.ContentTypeToolResult:
				if role != "assistant" {
					return nil, fmt.Errorf("openai-codex tool result appears in %s message", role)
				}
				value, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok {
					return nil, fmt.Errorf("openai-codex cannot encode tool result part %T", part)
				}
				flushContent()
				output, err := encodeToolOutput(value.Output)
				if err != nil {
					return nil, err
				}
				input = append(input, map[string]any{
					"type":    "function_call_output",
					"call_id": sanitizeCallID(value.ToolCallID),
					"output":  output,
				})
			case fantasy.ContentTypeReasoning:
				// Reasoning traces from another turn are not valid Codex history items.
			default:
				return nil, fmt.Errorf("openai-codex cannot encode message part %T", part)
			}
		}
		flushContent()
	}

	body := map[string]any{
		"model":   modelID,
		"input":   input,
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}
	if len(call.Tools) > 0 {
		tools := make([]any, 0, len(call.Tools))
		for _, tool := range call.Tools {
			function, ok := tool.(fantasy.FunctionTool)
			if !ok {
				if functionPtr, pointerOK := tool.(*fantasy.FunctionTool); pointerOK && functionPtr != nil {
					function = *functionPtr
					ok = true
				}
			}
			if !ok {
				return nil, fmt.Errorf("openai-codex supports function tools, not %T", tool)
			}
			parameters := function.InputSchema
			if parameters == nil {
				parameters = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        function.Name,
				"description": function.Description,
				"parameters":  parameters,
			})
		}
		body["tools"] = tools
		body["parallel_tool_calls"] = true
	}
	if call.ToolChoice != nil {
		choice := string(*call.ToolChoice)
		switch choice {
		case "auto", "none", "required":
			body["tool_choice"] = choice
		default:
			body["tool_choice"] = map[string]any{"type": "function", "name": choice}
		}
	}
	if effort, ok := ctx.Value(cliprovider.ReasoningEffortContextKey).(string); ok && strings.TrimSpace(effort) != "" {
		normalized := normalizeEffort(effort)
		if normalized == "" {
			return nil, fmt.Errorf("unsupported Codex reasoning effort %q", effort)
		}
		body["reasoning"] = map[string]any{"effort": normalized, "summary": "auto"}
	}
	return body, nil
}

func normalizeEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "off", "none", "disabled":
		return "none"
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	case "ultra":
		return "max"
	default:
		return ""
	}
}

func encodeToolOutput(output fantasy.ToolResultOutputContent) (any, error) {
	if text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](output); ok {
		return text.Text, nil
	}
	if outputErr, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](output); ok {
		if outputErr.Error == nil {
			return "", nil
		}
		return outputErr.Error.Error(), nil
	}
	if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](output); ok {
		if strings.HasPrefix(strings.ToLower(media.MediaType), "image/") {
			parts := make([]any, 0, 2)
			if media.Text != "" {
				parts = append(parts, map[string]any{"type": "input_text", "text": media.Text})
			}
			parts = append(parts, map[string]any{"type": "input_image", "image_url": "data:" + media.MediaType + ";base64," + media.Data})
			return parts, nil
		}
		return nil, fmt.Errorf("openai-codex cannot replay tool media type %q", media.MediaType)
	}
	return nil, fmt.Errorf("openai-codex cannot encode tool result %T", output)
}

func sanitizeCallID(raw string) string {
	base := raw
	if separator := strings.IndexAny(raw, "\n|"); separator > 0 {
		base = raw[:separator]
	} else if separator == 0 {
		base = raw[1:]
	}
	empty := base == ""
	if empty {
		base = "empty"
	}

	var sanitized strings.Builder
	for _, r := range base {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			sanitized.WriteRune(r)
		} else {
			sanitized.WriteByte('_')
		}
	}
	clean := strings.TrimRight(sanitized.String(), "_")
	if clean != "" && clean == base && len(clean) <= 64 && !empty {
		return clean
	}
	digest := sha256.Sum256([]byte(base))
	hash := hex.EncodeToString(digest[:4])
	if clean == "" {
		clean = "call"
	}
	prefixLength := min(len(clean), 63-len(hash))
	return strings.TrimRight(clean[:prefixLength], "_") + "_" + hash
}

func (m *languageModel) stream(ctx context.Context, payload []byte, userAgent string, yield func(fantasy.StreamPart) bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, responsesURL, bytes.NewReader(payload))
	if err != nil {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		return
	}
	request.Header.Set("Authorization", "Bearer "+m.provider.token.AccessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("OpenAI-Beta", "responses=experimental")
	request.Header.Set("originator", "omp")
	request.Header.Set("version", codexVersion)
	if strings.TrimSpace(userAgent) == "" {
		userAgent = "rush"
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("x-codex-routing-hint", "model="+m.modelID)
	if accountID := strings.TrimSpace(m.provider.token.AccountID); accountID != "" {
		request.Header.Set("chatgpt-account-id", accountID)
	}
	if sessionID, ok := ctx.Value(cliprovider.SessionIDContextKey).(string); ok && sessionID != "" {
		request.Header.Set("conversation_id", sessionID)
		request.Header.Set("session_id", sessionID)
		request.Header.Set("x-client-request-id", sessionID)
	}

	response, err := m.provider.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		return
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		if readErr != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: readErr})
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: providerError(response, payload, body)})
		return
	}

	state := newStreamState()
	readErr := readSSE(response.Body, func(data []byte) bool {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return true
		}
		if err := state.consume(data, yield); err != nil {
			state.err = err
			return false
		}
		return true
	})
	if errors.Is(readErr, errStreamStopped) {
		if state.err != nil && !errors.Is(state.err, errStreamStopped) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: state.err})
		}
		return
	}
	if readErr != nil {
		if ctx.Err() != nil {
			readErr = ctx.Err()
		} else if errors.Is(readErr, io.ErrUnexpectedEOF) {
			readErr = streamTransportError(readErr)
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: readErr})
		return
	}
	if !state.completed {
		if ctx.Err() != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
		} else {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fantasy.NewIncompleteStreamError()})
		}
		return
	}
	state.finish(yield)
}

// streamTransportError wraps a truncated stream body so retry classification
// sees a retryable provider error instead of a terminal raw EOF.
func streamTransportError(err error) error {
	return &fantasy.ProviderError{Title: "stream transport error", Message: err.Error(), Cause: err}
}

func readSSE(reader io.Reader, yield func([]byte) bool) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxSSEEventSize)
	var data bytes.Buffer
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if data.Len() > 0 {
				payload := bytes.TrimSuffix(data.Bytes(), []byte("\n"))
				if !yield(payload) {
					return errStreamStopped
				}
				data.Reset()
			}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, found := bytes.Cut(line, []byte(":"))
		if !found || !bytes.Equal(field, []byte("data")) {
			continue
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		data.Write(value)
		data.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if data.Len() > 0 && !yield(bytes.TrimSuffix(data.Bytes(), []byte("\n"))) {
		return errStreamStopped
	}
	return nil
}

func providerError(response *http.Response, requestBody, body []byte) error {
	message := strings.TrimSpace(string(body))
	var decoded struct {
		Error codexFailure `json:"error"`
	}
	if json.Unmarshal(body, &decoded) == nil && decoded.Error.Message != "" {
		message = codexFailureMessage(decoded.Error, message)
	}
	if message == "" {
		message = response.Status
	}
	headers := make(map[string]string, len(response.Header))
	for key, values := range response.Header {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
	return &fantasy.ProviderError{
		Message:         message,
		Title:           fantasy.ErrorTitleForStatusCode(response.StatusCode),
		URL:             responsesURL,
		StatusCode:      response.StatusCode,
		RequestBody:     append([]byte(nil), requestBody...),
		ResponseHeaders: headers,
		ResponseBody:    append([]byte(nil), body...),
	}
}

type toolCall struct {
	id        string
	itemID    string
	name      string
	arguments strings.Builder
	started   bool
	finished  bool
}

type streamState struct {
	textStarted      bool
	reasoningStarted bool
	textSeen         bool
	reasoningSeen    bool
	completed        bool
	finishReason     fantasy.FinishReason
	usage            fantasy.Usage
	tools            map[string]*toolCall
	toolOrder        []string
	err              error
}

func newStreamState() *streamState {
	return &streamState{tools: make(map[string]*toolCall), finishReason: fantasy.FinishReasonStop}
}

func (s *streamState) consume(data []byte, yield func(fantasy.StreamPart) bool) error {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(data, &event); err != nil {
		return fmt.Errorf("decode Codex SSE event: %w", err)
	}
	kind := rawString(event["type"])
	switch kind {
	case "response.output_text.delta":
		delta := rawString(event["delta"])
		if delta != "" {
			s.textSeen = true
			if !s.textStarted {
				s.textStarted = true
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"}) {
					return errStreamStopped
				}
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: delta}) {
				return errStreamStopped
			}
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := rawString(event["delta"])
		if delta != "" {
			s.reasoningSeen = true
			if !s.reasoningStarted {
				s.reasoningStarted = true
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: "reasoning"}) {
					return errStreamStopped
				}
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "reasoning", Delta: delta}) {
				return errStreamStopped
			}
		}
	case "response.output_item.added", "response.output_item.done":
		var item struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if raw := event["item"]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &item); err != nil {
				return fmt.Errorf("decode Codex output item: %w", err)
			}
		}
		if item.Type == "function_call" {
			call := s.getTool(item.ID, parseOutputIndex(event["output_index"]))
			call.itemID = item.ID
			if item.CallID != "" {
				call.id = sanitizeCallID(item.CallID)
			} else if call.id == "" {
				call.id = sanitizeCallID(item.ID)
			}
			call.name = item.Name
			if !call.started {
				call.started = true
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: call.id, ToolCallName: call.name}) {
					return errStreamStopped
				}
			}
			if kind == "response.output_item.done" && item.Arguments != "" {
				if err := call.setFinalArguments(item.Arguments, yield); err != nil {
					return err
				}
			}
			if kind == "response.output_item.done" {
				if err := s.finishTool(call, yield); err != nil {
					return err
				}
			}
		}
	case "response.function_call_arguments.delta":
		key := rawString(event["item_id"])
		call := s.getTool(key, parseOutputIndex(event["output_index"]))
		if call.itemID == "" {
			call.itemID = key
		}
		if call.id == "" {
			call.id = sanitizeCallID(key)
		}
		if !call.started {
			call.started = true
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: call.id, ToolCallName: call.name}) {
				return errStreamStopped
			}
		}
		delta := rawString(event["delta"])
		if delta != "" {
			call.arguments.WriteString(delta)
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: call.id, Delta: delta}) {
				return errStreamStopped
			}
		}
	case "response.function_call_arguments.done":
		key := rawString(event["item_id"])
		call := s.getTool(key, parseOutputIndex(event["output_index"]))
		if call.itemID == "" {
			call.itemID = key
		}
		if call.id == "" {
			call.id = sanitizeCallID(key)
		}
		if !call.started {
			call.started = true
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: call.id, ToolCallName: call.name}) {
				return errStreamStopped
			}
		}
		if args := rawString(event["arguments"]); args != "" {
			if err := call.setFinalArguments(args, yield); err != nil {
				return err
			}
		}
		if err := s.finishTool(call, yield); err != nil {
			return err
		}
	case "response.completed", "response.incomplete":
		response, err := decodeResponse(event["response"])
		if err != nil {
			return err
		}
		if !s.reasoningSeen && response.reasoning != "" {
			s.reasoningSeen = true
			s.reasoningStarted = true
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: "reasoning"}) {
				return errStreamStopped
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: "reasoning", Delta: response.reasoning}) {
				return errStreamStopped
			}
		}
		if !s.textSeen && response.text != "" {
			s.textSeen = true
			s.textStarted = true
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"}) {
				return errStreamStopped
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: response.text}) {
				return errStreamStopped
			}
		}
		for _, item := range response.output {
			if item.Type == "function_call" {
				call := s.getTool(item.ID, "")
				call.itemID = item.ID
				if item.CallID != "" {
					call.id = sanitizeCallID(item.CallID)
				} else if call.id == "" {
					call.id = sanitizeCallID(item.ID)
				}
				call.name = item.Name
				if item.Arguments != "" {
					if err := call.setFinalArguments(item.Arguments, yield); err != nil {
						return err
					}
				}
				if err := s.finishTool(call, yield); err != nil {
					return err
				}
			}
		}
		s.usage = response.usage
		s.finishReason = response.finishReason
		if kind == "response.incomplete" || response.status == "incomplete" {
			if s.finishReason == fantasy.FinishReasonStop {
				s.finishReason = fantasy.FinishReasonLength
			}
		} else if s.finishReasonHasTools() {
			s.finishReason = fantasy.FinishReasonToolCalls
		}
		s.completed = true
	case "response.failed", "error":
		return codexStreamFailure(data)
	case "rush.stream_error":
		return fmt.Errorf("read Codex event stream: %s", rawString(event["message"]))
	}
	return nil
}

// streamFailureStatus maps the code of a failed Codex stream to the HTTP
// status the same condition would carry before the stream starts, so the
// retry classifiers treat a server-side hiccup like one (an overloaded or
// failing backend is worth a re-run) instead of a terminal provider verdict:
// the worker turn that met server_is_overloaded used to die without a retry.
// Unknown codes keep status 0, which stays terminal.
func streamFailureStatus(code string) int {
	switch code {
	case "server_is_overloaded", "overloaded", "service_unavailable":
		return http.StatusServiceUnavailable
	case "server_error", "internal_server_error", "internal_error":
		return http.StatusInternalServerError
	case "rate_limit_exceeded":
		return http.StatusTooManyRequests
	}
	return 0
}

var errStreamStopped = errors.New("codex stream consumer stopped")

func (s *streamState) getTool(itemID, outputIndex string) *toolCall {
	key := itemID
	if key == "" {
		key = "index:" + outputIndex
	}
	if key == "" {
		key = "unknown"
	}
	call := s.tools[key]
	if call == nil {
		call = &toolCall{}
		s.tools[key] = call
		s.toolOrder = append(s.toolOrder, key)
	}
	return call
}

func (call *toolCall) setFinalArguments(arguments string, yield func(fantasy.StreamPart) bool) error {
	current := call.arguments.String()
	if arguments == current {
		return nil
	}
	if strings.HasPrefix(arguments, current) {
		delta := arguments[len(current):]
		if delta != "" {
			call.arguments.WriteString(delta)
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: call.id, Delta: delta}) {
				return errStreamStopped
			}
		}
		return nil
	}
	call.arguments.Reset()
	call.arguments.WriteString(arguments)
	return nil
}

func (s *streamState) finishTool(call *toolCall, yield func(fantasy.StreamPart) bool) error {
	if call.finished {
		return nil
	}
	if call.id == "" {
		return errors.New("codex function call is missing a call ID")
	}
	if call.name == "" {
		return errors.New("codex function call is missing a name")
	}
	if !call.started {
		call.started = true
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: call.id, ToolCallName: call.name}) {
			return errStreamStopped
		}
	}
	args := call.arguments.String()
	if args == "" {
		args = "{}"
	}
	if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: call.id}) {
		return errStreamStopped
	}
	if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: call.id, ToolCallName: call.name, ToolCallInput: args}) {
		return errStreamStopped
	}
	call.finished = true
	return nil
}

func (s *streamState) finish(yield func(fantasy.StreamPart) bool) {
	if s.textStarted && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"}) {
		return
	}
	if s.reasoningStarted && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: "reasoning"}) {
		return
	}
	if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: s.finishReason, Usage: s.usage}) {
		return
	}
}

type responseOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseOutputItem struct {
	ID        string                  `json:"id"`
	Type      string                  `json:"type"`
	CallID    string                  `json:"call_id"`
	Name      string                  `json:"name"`
	Arguments string                  `json:"arguments"`
	Content   []responseOutputContent `json:"content"`
	Summary   []responseOutputContent `json:"summary"`
}

type decodedResponse struct {
	status       string
	text         string
	reasoning    string
	output       []responseOutputItem
	usage        fantasy.Usage
	finishReason fantasy.FinishReason
}

func decodeResponse(raw json.RawMessage) (decodedResponse, error) {
	var wire struct {
		Status string               `json:"status"`
		Output []responseOutputItem `json:"output"`
		Usage  struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
			InputDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &wire); err != nil {
			return decodedResponse{}, fmt.Errorf("decode Codex response: %w", err)
		}
	}
	decoded := decodedResponse{
		status: wire.Status,
		output: wire.Output,
		usage: fantasy.Usage{
			InputTokens:     wire.Usage.InputTokens,
			OutputTokens:    wire.Usage.OutputTokens,
			TotalTokens:     wire.Usage.TotalTokens,
			CacheReadTokens: wire.Usage.InputDetails.CachedTokens,
			ReasoningTokens: wire.Usage.OutputDetails.ReasoningTokens,
		},
		finishReason: fantasy.FinishReasonStop,
	}
	if decoded.usage.TotalTokens == 0 {
		decoded.usage.TotalTokens = decoded.usage.InputTokens + decoded.usage.OutputTokens
	}
	switch wire.IncompleteDetails.Reason {
	case "max_output_tokens":
		decoded.finishReason = fantasy.FinishReasonLength
	case "content_filter":
		decoded.finishReason = fantasy.FinishReasonContentFilter
	}
	for _, item := range wire.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				if part.Type == "output_text" {
					decoded.text += part.Text
				}
			}
		case "reasoning":
			for _, part := range item.Summary {
				if part.Type == "summary_text" {
					decoded.reasoning += part.Text
				}
			}
		}
	}
	return decoded, nil
}

func rawString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func (s *streamState) finishReasonHasTools() bool {
	for _, key := range s.toolOrder {
		if call := s.tools[key]; call != nil && call.finished {
			return true
		}
	}
	return false
}

func (m *languageModel) String() string { return providerID + "/" + m.modelID }

func parseOutputIndex(raw json.RawMessage) string {
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strconv.Itoa(value)
}

var (
	_ fantasy.Provider      = (*provider)(nil)
	_ fantasy.LanguageModel = (*languageModel)(nil)
)
