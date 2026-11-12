package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesTransport controls only direct Codex and public OpenAI Responses.
type ResponsesTransport string

const (
	ResponsesTransportAuto ResponsesTransport = "auto"
	ResponsesTransportHTTP ResponsesTransport = "http"
)

func (p *ResponsesTransport) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*p = ""
		return nil
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	switch ResponsesTransport(value) {
	case ResponsesTransportAuto, ResponsesTransportHTTP:
		*p = ResponsesTransport(value)
		return nil
	default:
		return fmt.Errorf("responses_transport must be auto or http")
	}
}

func validateResponsesTransportWrite(raw []byte, keys []string) error {
	for _, key := range keys {
		parts := strings.Split(key, ".")
		touched := key == "options" || key == "options.responses_transport" || strings.HasPrefix(key, "options.responses_transport.") || key == "providers" ||
			(len(parts) >= 2 && parts[0] == "providers" && (len(parts) == 2 || parts[2] == "responses_transport"))
		if !touched {
			continue
		}
		var document struct {
			Options struct {
				Transport ResponsesTransport `json:"responses_transport"`
			} `json:"options"`
			Providers map[string]struct {
				Transport ResponsesTransport `json:"responses_transport"`
			} `json:"providers"`
		}
		return json.Unmarshal(raw, &document)
	}
	return nil
}

func EffectiveResponsesTransport(global, provider ResponsesTransport) ResponsesTransport {
	if provider != "" {
		return provider
	}
	if global != "" {
		return global
	}
	return ResponsesTransportAuto
}
