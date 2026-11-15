package codexprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"charm.land/fantasy"
)

func (m *languageModel) stream(ctx context.Context, payload []byte, userAgent string, yield func(fantasy.StreamPart) bool) {
	obs := newTransportObservation(m.modelID)
	defer func() { obs.log(ctx) }()
	yield = obs.wrap(yield)
	if m.streamWebSocket(ctx, payload, userAgent, yield, obs) {
		return
	}
	response, err := m.acquireResponse(ctx, payload, userAgent)
	if err != nil {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
		return
	}
	defer response.Body.Close()

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
	acceptedOutput   bool
	completed        bool
	finishReason     fantasy.FinishReason
	usage            fantasy.Usage
	metadata         fantasy.ProviderMetadata
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
	case "response.failed", "error":
		if codexEventHasOutput(event) {
			s.acceptedOutput = true
		}
	case "rush.stream_error", "response.created", "response.in_progress", "response.queued":
		if codexEventHasOutput(event) {
			s.acceptedOutput = true
		}
		// Lifecycle-only failures may still be safely retried.
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		// Empty text/reasoning deltas carry no accepted content.
	case "response.output_item.added", "response.output_item.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.completed", "response.incomplete":
		s.acceptedOutput = true
	default:
		// Unknown/ambiguous events fail closed: they might represent accepted
		// provider output the local decoder does not understand.
		s.acceptedOutput = true
	}
	switch kind {
	case "response.output_text.delta":
		delta := rawString(event["delta"])
		if delta != "" {
			s.acceptedOutput = true
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
			s.acceptedOutput = true
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
		s.acceptedOutput = true
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
		s.acceptedOutput = true
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
			s.acceptedOutput = true
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
			s.acceptedOutput = true
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
			if item.Type != "" && item.Type != "function_call" && item.Type != "message" && item.Type != "reasoning" {
				s.acceptedOutput = true
			}
			if item.Type == "function_call" {
				s.acceptedOutput = true
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
		s.metadata = response.metadata
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
		return codexStreamFailure(data, !s.acceptedOutput)
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
	if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: s.finishReason, Usage: s.usage, ProviderMetadata: s.metadata}) {
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
	metadata     fantasy.ProviderMetadata
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
				CachedTokens *int64 `json:"cached_tokens"`
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
	cachedTokens := int64(0)
	if wire.Usage.InputDetails.CachedTokens != nil {
		cachedTokens = *wire.Usage.InputDetails.CachedTokens
	}
	decoded := decodedResponse{
		status: wire.Status,
		output: wire.Output,
		usage: fantasy.Usage{
			// Responses input includes cached tokens; fantasy uses disjoint counters.
			InputTokens:     max(wire.Usage.InputTokens-cachedTokens, 0),
			OutputTokens:    wire.Usage.OutputTokens,
			TotalTokens:     wire.Usage.TotalTokens,
			CacheReadTokens: cachedTokens,
			ReasoningTokens: wire.Usage.OutputDetails.ReasoningTokens,
		},
		finishReason: fantasy.FinishReasonStop,
	}
	if wire.Usage.InputDetails.CachedTokens != nil {
		decoded.metadata = fantasy.ProviderMetadata{Name: &UsageMetadata{CachedTokens: wire.Usage.InputDetails.CachedTokens}}
	}
	if decoded.usage.TotalTokens == 0 {
		decoded.usage.TotalTokens = wire.Usage.InputTokens + wire.Usage.OutputTokens
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

func parseOutputIndex(raw json.RawMessage) string {
	var value int
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strconv.Itoa(value)
}
