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

func isContextLengthExceeded(failure codexFailure) bool {
	return failure.Code == "context_length_exceeded" || failure.Type == "context_length_exceeded"
}

func codexStreamFailure(data []byte) error {
	var failure struct {
		Error    codexFailure `json:"error"`
		Response struct {
			Error codexFailure `json:"error"`
		} `json:"response"`
	}
	_ = json.Unmarshal(data, &failure)
	selected := failure.Error
	if selected.Message == "" {
		selected = failure.Response.Error
	}
	code := selected.Code
	if code == "" {
		code = selected.Type
	}
	return &fantasy.ProviderError{
		Message: codexFailureMessage(selected, strings.TrimSpace(string(data))),
		Title:   "Codex response error", StatusCode: streamFailureStatus(code),
		URL: responsesURL, ResponseBody: append([]byte(nil), data...),
		ContextTooLargeErr: isContextLengthExceeded(selected),
	}
}
