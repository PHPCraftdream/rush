// Package codexprovider implements direct access to ChatGPT's Codex Responses API.
package codexprovider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/object"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth"
)

const (
	// Name identifies the direct Codex Responses provider.
	Name            = "openai-codex"
	responsesURL    = "https://chatgpt.com/backend-api/codex/responses"
	codexVersion    = "0.159.0"
	maxErrorBody    = 1 << 20
	maxSSEEventSize = 4 << 20
)

type provider struct {
	client    *http.Client
	source    TokenSource
	websocket *WebSocketOptions
}

// New creates a provider that authenticates only with a ChatGPT OAuth token.
func New(client *http.Client, token *oauth.Token) (fantasy.Provider, error) {
	if token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("openai-codex requires an OAuth access token")
	}
	return NewWithSource(client, staticTokenSource{token: *token})
}

func (p *provider) Name() string { return Name }

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

func (m *languageModel) Provider() string { return Name }
func (m *languageModel) Model() string    { return m.modelID }

func (m *languageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return object.GenerateWithTool(ctx, m, call)
}

func (m *languageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	acceptance := &websocketObjectAcceptance{}
	stream, err := object.StreamWithTool(context.WithValue(ctx, websocketObjectAcceptanceKey{}, acceptance), m, call)
	if err != nil {
		acceptance.resolve(false)
		return nil, err
	}
	stopCancel := context.AfterFunc(ctx, func() { acceptance.resolve(false) })
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		accepted, finished := false, false
		defer func() {
			stopCancel()
			acceptance.resolve(accepted && ctx.Err() == nil)
		}()
		for part := range stream {
			if finished {
				return
			}
			if part.Type == fantasy.ObjectStreamPartTypeFinish {
				finished = true
			}
			if !yield(part) {
				return
			}
			if part.Type == fantasy.ObjectStreamPartTypeError {
				return
			}
		}
		accepted = finished
	}, nil
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
	var metadata fantasy.ProviderMetadata
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
			metadata = part.ProviderMetadata
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
	return &fantasy.Response{Content: content, FinishReason: finishReason, Usage: usage, ProviderMetadata: metadata}, nil
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
	if cacheKey, ok := promptCacheKey(ctx, call); ok {
		body["prompt_cache_key"] = cacheKey
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
		Message:            message,
		Title:              fantasy.ErrorTitleForStatusCode(response.StatusCode),
		URL:                responsesURL,
		StatusCode:         response.StatusCode,
		RequestBody:        append([]byte(nil), requestBody...),
		ResponseHeaders:    headers,
		ResponseBody:       append([]byte(nil), body...),
		ContextTooLargeErr: isContextLengthExceeded(decoded.Error),
	}
}

var errStreamStopped = errors.New("codex stream consumer stopped")

func (m *languageModel) String() string { return Name + "/" + m.modelID }

var (
	_ fantasy.Provider      = (*provider)(nil)
	_ fantasy.LanguageModel = (*languageModel)(nil)
)
