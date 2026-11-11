package codexprovider

import (
	"encoding/json"
	"strings"

	"charm.land/fantasy"
)

type codexFailure struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

func codexFailureMessage(failure codexFailure, fallback string) string {
	message := failure.Message
	if message == "" {
		message = fallback
	}
	code := failure.Code
	if code == "" {
		code = failure.Type
	}
	if code != "" {
		message = code + ": " + message
	}
	return message
}

func codexEventHasOutput(event map[string]json.RawMessage) bool {
	if rawHasOutputValue(event["item"]) || rawHasOutputValue(event["output_item"]) || rawHasOutputValue(event["output"]) || rawHasOutputValue(event["usage"]) || rawHasOutputValue(event["output_text"]) || rawHasOutputValue(event["text"]) || rawHasOutputValue(event["reasoning"]) || rawHasOutputValue(event["encrypted_content"]) || rawHasOutputValue(event["content"]) || rawHasOutputValue(event["summary"]) || rawHasOutputValue(event["arguments"]) || rawHasOutputValue(event["delta"]) {
		return true
	}
	var response map[string]json.RawMessage
	if len(event["response"]) == 0 || json.Unmarshal(event["response"], &response) != nil {
		return len(event["response"]) > 0 && string(event["response"]) != "null"
	}
	return rawHasOutputValue(response["item"]) || rawHasOutputValue(response["output_item"]) || rawHasOutputValue(response["output"]) || rawHasOutputValue(response["usage"]) || rawHasOutputValue(response["output_text"]) || rawHasOutputValue(response["text"]) || rawHasOutputValue(response["reasoning"]) || rawHasOutputValue(response["encrypted_content"]) || rawHasOutputValue(response["content"]) || rawHasOutputValue(response["summary"]) || rawHasOutputValue(response["arguments"]) || rawHasOutputValue(response["delta"])
}

func rawHasOutputValue(raw json.RawMessage) bool {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return false
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) == nil {
		return len(values) > 0
	}
	return true
}

func isContextLengthExceeded(failure codexFailure) bool {
	return failure.Code == "context_length_exceeded" || failure.Type == "context_length_exceeded"
}

func codexStreamFailure(data []byte, safeBeforeOutput ...bool) error {
	var failure struct {
		Type     string       `json:"type"`
		Error    codexFailure `json:"error"`
		Response struct {
			Error codexFailure `json:"error"`
		} `json:"response"`
	}
	_ = json.Unmarshal(data, &failure)
	selected := failure.Error
	if selected.Message == "" && selected.Code == "" && selected.Type == "" {
		selected = failure.Response.Error
	}
	code := selected.Code
	if code == "" {
		code = selected.Type
	}
	providerErr := &fantasy.ProviderError{
		Message: codexFailureMessage(selected, strings.TrimSpace(string(data))),
		Title:   "Codex response error", StatusCode: streamFailureStatus(code),
		URL: responsesURL, ResponseBody: append([]byte(nil), data...),
		ContextTooLargeErr: isContextLengthExceeded(selected),
	}
	certifiedBeforeOutput := false
	if len(safeBeforeOutput) > 0 && safeBeforeOutput[0] && failure.Type == "response.failed" {
		var event map[string]json.RawMessage
		_ = json.Unmarshal(data, &event)
		certifiedBeforeOutput = !codexEventHasOutput(event)
	}
	if certifiedBeforeOutput && providerErr.IsContextTooLarge() {
		return &safeContextRejection{ProviderError: providerErr}
	}
	return providerErr
}

type safeContextRejection struct{ *fantasy.ProviderError }

func (e *safeContextRejection) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.ProviderError
}

func (e *safeContextRejection) CodexContextRejectedBeforeOutput() bool {
	return e != nil && e.IsContextTooLarge()
}
