package codexprovider

import (
	"bytes"
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
	if rawHasOutputValue(response["item"]) || rawHasOutputValue(response["output_item"]) || rawHasOutputValue(response["output"]) || rawHasOutputValue(response["usage"]) || rawHasOutputValue(response["output_text"]) || rawHasOutputValue(response["encrypted_content"]) || rawHasOutputValue(response["content"]) || rawHasOutputValue(response["summary"]) || rawHasOutputValue(response["arguments"]) || rawHasOutputValue(response["delta"]) {
		return true
	}
	return rawHasOutputValue(response["reasoning"]) && !codexReasoningOptions(response["reasoning"]) ||
		rawHasOutputValue(response["text"]) && !codexTextOptions(response["text"])
}

func codexReasoningOptions(raw json.RawMessage) bool {
	var options map[string]json.RawMessage
	if json.Unmarshal(raw, &options) != nil || len(options) == 0 {
		return false
	}
	for key, value := range options {
		var choices []string
		switch key {
		case "effort":
			choices = []string{"none", "minimal", "low", "medium", "high", "xhigh"}
		case "summary", "generate_summary":
			choices = []string{"auto", "concise", "detailed"}
		default:
			return false
		}
		if !codexStringOption(value, choices...) {
			return false
		}
	}
	return true
}

func codexTextOptions(raw json.RawMessage) bool {
	var options map[string]json.RawMessage
	if json.Unmarshal(raw, &options) != nil || len(options) == 0 {
		return false
	}
	for key, value := range options {
		switch key {
		case "format":
			var format map[string]json.RawMessage
			if json.Unmarshal(value, &format) != nil || len(format) != 1 {
				return false
			}
			var formatType string
			if json.Unmarshal(format["type"], &formatType) != nil || formatType != "text" {
				return false
			}
		case "verbosity":
			if !codexStringOption(value, "low", "medium", "high") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func codexStringOption(raw json.RawMessage, choices ...string) bool {
	if strings.TrimSpace(string(raw)) == "null" {
		return true
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func codexErrorEventHasKnownShape(event map[string]json.RawMessage) bool {
	for key, value := range event {
		switch key {
		case "type", "message":
			if !isJSONString(value) {
				return false
			}
		case "code":
			if !isJSONString(value) {
				return false
			}
		case "param":
			if strings.TrimSpace(string(value)) != "null" && !isJSONString(value) {
				return false
			}
		case "sequence_number":
			var sequence int64
			if json.Unmarshal(value, &sequence) != nil || sequence < 0 {
				return false
			}
		case "error":
			var failure map[string]json.RawMessage
			if json.Unmarshal(value, &failure) != nil || failure == nil || len(failure) == 0 || event["code"] != nil || event["message"] != nil || event["param"] != nil {
				return false
			}
			for key, nestedValue := range failure {
				switch key {
				case "code", "type", "message":
					if !isJSONString(nestedValue) {
						return false
					}
				case "param":
					if strings.TrimSpace(string(nestedValue)) != "null" && !isJSONString(nestedValue) {
						return false
					}
				default:
					return false
				}
			}
		default:
			return false
		}
	}
	return event["error"] != nil || event["code"] != nil
}

func isJSONString(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '"'
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
		Code     string       `json:"code"`
		Message  string       `json:"message"`
		Error    codexFailure `json:"error"`
		Response struct {
			Error codexFailure `json:"error"`
		} `json:"response"`
	}
	decodeErr := json.Unmarshal(data, &failure)
	selected := failure.Error
	if selected.Message == "" && selected.Code == "" && selected.Type == "" && failure.Type == "response.failed" {
		selected = failure.Response.Error
	}
	if selected.Message == "" && selected.Code == "" && selected.Type == "" && failure.Type == "error" {
		selected = codexFailure{Code: failure.Code, Message: failure.Message}
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
	if len(safeBeforeOutput) > 0 && safeBeforeOutput[0] && decodeErr == nil && (failure.Type == "response.failed" || failure.Type == "error") {
		var event map[string]json.RawMessage
		if json.Unmarshal(data, &event) != nil {
			return providerErr
		}
		certifiedBeforeOutput = !codexEventHasOutput(event)
		if failure.Type == "error" {
			certifiedBeforeOutput = certifiedBeforeOutput && codexErrorEventHasKnownShape(event)
		}
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
