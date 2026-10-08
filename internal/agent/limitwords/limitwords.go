// Package limitwords exposes Classify(Input) for #1283/#1284 adapters.
// Input.Body is a raw JSON failure envelope or HTTP response dump string.
// Classify returns Unknown, Transient or Hard without repository dependencies.
package limitwords

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

type Class uint8

const (
	Unknown Class = iota
	Transient
	Hard
)

type Input struct {
	Status                   int
	Title, Message, Provider string
	Body                     string
	ResetAfter               time.Duration
}

var (
	hardLimitKeywords      = []string{"usage limit", "limit will reset", "reset at", "quota", "you've hit your limit"}
	transientLimitKeywords = []string{"per minute", "tpm", "rpm", "requests", "overloaded", "try again later", "rate_limit_exceeded"}
	zaiHardLimitCodes      = map[string]bool{"1113": true, "1308": true, "1309": true, "1310": true, "1316": true, "1317": true, "1318": true, "1319": true, "1320": true, "1321": true}
)

func normalizeLimitText(text string) string {
	return strings.ToLower(strings.NewReplacer("’", "'", "‘", "'").Replace(text))
}

// ErrorObject extracts failure fields from JSON envelopes or HTTP response dumps.
func ErrorObject(body string) map[string]any {
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "HTTP/") {
		if _, rest, ok := strings.Cut(body, "\r\n\r\n"); ok {
			body = rest
		} else {
			return nil
		}
	}
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	if decoder.Decode(&root) != nil {
		return nil
	}
	if obj, ok := root["error"].(map[string]any); ok {
		return obj
	}
	if response, ok := root["response"].(map[string]any); ok {
		if obj, ok := response["error"].(map[string]any); ok {
			return obj
		}
	}
	return nil
}

func Field(obj map[string]any, key string) string {
	switch value := obj[key].(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	}
	return ""
}

func IsZAICode(code string) bool { return zaiHardLimitCodes[code] || code == "1302" || code == "1305" }

func Classify(input Input) Class {
	provider := normalizeLimitText(input.Provider)
	obj := ErrorObject(input.Body)
	text := normalizeLimitText(input.Title + " " + input.Message + " " + Field(obj, "message"))
	if (input.Status == 400 || input.Status == 402) && strings.Contains(text, "your credit balance is too low to access the anthropic api") {
		return Hard
	}
	if input.Status == 402 && provider == "stepfun" {
		return Hard
	}
	codexWall := provider == "openai-codex" && (strings.EqualFold(Field(obj, "type"), "usage_limit_reached") || strings.EqualFold(Field(obj, "code"), "usage_limit_reached") || strings.Contains(text, "usage_limit_reached"))
	if input.Status != http.StatusTooManyRequests && !(input.Status == 0 && codexWall) {
		return Unknown
	}
	if input.ResetAfter >= 30*time.Minute {
		return Hard
	}
	if provider == "stepfun" {
		return Transient
	}
	if provider == "zai" {
		code := Field(obj, "code")
		if code == "1302" || code == "1305" {
			return Transient
		}
		if zaiHardLimitCodes[code] {
			return Hard
		}
	}
	if codexWall {
		return Hard
	}
	failureText := normalizeLimitText(input.Message + " " + Field(obj, "message") + " " + Field(obj, "code") + " " + Field(obj, "type"))
	for _, keyword := range transientLimitKeywords {
		if strings.Contains(failureText, keyword) {
			return Transient
		}
	}
	for _, keyword := range hardLimitKeywords {
		if strings.Contains(text, keyword) {
			return Hard
		}
	}
	return Unknown
}
