package agent

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/limitwords"
	"google.golang.org/genai"
)

type providerLimitClass = limitwords.Class

const (
	providerLimitUnknown   = limitwords.Unknown
	providerLimitTransient = limitwords.Transient
	providerLimitHard      = limitwords.Hard
)

func limitErrorObject(body []byte) map[string]any { return limitwords.ErrorObject(string(body)) }

func limitField(obj map[string]any, key string) string { return limitwords.Field(obj, key) }

// Provider hints are optional; ProviderError itself has no provider ID.
func limitProvider(pe *fantasy.ProviderError, hint string, obj map[string]any) string {
	switch strings.ToLower(hint) {
	case "zai", "stepfun", "openai-codex":
		return strings.ToLower(hint)
	}
	if provider := limitwords.ProviderIdentity(pe.URL, hint); provider != "" {
		return provider
	}
	text := strings.ToLower(pe.Title + " " + pe.Message + " " + limitField(obj, "message"))
	if strings.Contains(text, "stepfun") || strings.Contains(text, "step plan") {
		return "stepfun"
	}
	if strings.Contains(text, "codex") || strings.EqualFold(limitField(obj, "type"), "usage_limit_reached") || strings.EqualFold(limitField(obj, "code"), "usage_limit_reached") {
		return "openai-codex"
	}
	code := limitField(obj, "code")
	if limitwords.IsZAICode(code) {
		return "zai"
	}
	return ""
}

func structuredLimitReset(pe *fantasy.ProviderError, now time.Time) (time.Time, bool) {
	obj := limitErrorObject(pe.ResponseBody)
	if seconds, err := strconv.ParseFloat(limitField(obj, "resets_in_seconds"), 64); err == nil && seconds >= 0 {
		return now.Add(time.Duration(seconds * float64(time.Second))), true
	}
	if seconds, err := strconv.ParseInt(limitField(obj, "resets_at"), 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0), true
	}
	return time.Time{}, false
}

func hasLongLimitReset(pe *fantasy.ProviderError, now time.Time) bool {
	for key, value := range pe.ResponseHeaders {
		name := strings.ToLower(key)
		if name == "retry-after" || name == "retry-after-ms" {
			scale := float64(time.Second)
			if name == "retry-after-ms" {
				scale = float64(time.Millisecond)
			}
			if seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && seconds*scale >= float64(30*time.Minute) {
				return true
			}
		}
		copyError := *pe
		copyError.ResponseHeaders = map[string]string{key: value}
		copyError.Message = ""
		copyError.ResponseBody = nil
		if reset, ok := QuotaLimitResetTime(&copyError, now); ok && reset.Sub(now) >= 30*time.Minute {
			return true
		}
	}
	if reset, ok := parseZAIResetHint(pe.Message); ok && reset.Sub(now) >= 30*time.Minute {
		return true
	}
	obj := limitErrorObject(pe.ResponseBody)
	for _, field := range []string{"resets_in_seconds", "resets_at"} {
		copyObject := map[string]any{field: obj[field]}
		body, _ := json.Marshal(map[string]any{"error": copyObject})
		copyError := *pe
		copyError.ResponseBody = body
		if reset, ok := structuredLimitReset(&copyError, now); ok && reset.Sub(now) >= 30*time.Minute {
			return true
		}
	}
	if reset, ok := parseZAIResetHint(limitField(obj, "message")); ok && reset.Sub(now) >= 30*time.Minute {
		return true
	}
	return false
}

func classifyHardProviderLimit(err error, hint string, now time.Time) providerLimitClass {
	var pe *fantasy.ProviderError
	if !errors.As(err, &pe) || pe == nil || pe.IsContextTooLarge() {
		return providerLimitUnknown
	}
	// Fantasy retains Google RPC Details only in Cause; never mutate its error.
	var googleError genai.APIError
	if errors.As(pe.Cause, &googleError) {
		copyError := *pe
		body, marshalErr := json.Marshal(map[string]any{"error": googleError})
		if marshalErr == nil {
			copyError.ResponseBody = body
			pe = &copyError
			hint = "gemini"
		}
	}
	obj := limitErrorObject(pe.ResponseBody)
	provider := limitProvider(pe, hint, obj)
	var resetAfter time.Duration
	if hasLongLimitReset(pe, now) {
		resetAfter = 30 * time.Minute
	}
	return limitwords.Classify(limitwords.Input{Status: pe.StatusCode, Title: pe.Title, Message: pe.Message, Body: string(pe.ResponseBody), Provider: provider, ResetAfter: resetAfter})
}
