package limitwords

import (
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Rules are serving-provider scoped, never model-name scoped. Evidence: research
// rows 37–50 in docs/research/2026-10-08-provider-limit-errors.md.
type providerRule struct {
	provider       string
	statuses       []int
	codes, words   []string
	class          Class
	hosts, aliases []string
	hostPattern    string
}

var providerRules = []providerRule{
	{provider: "moonshot", hosts: []string{"api.moonshot.ai", "api.moonshot.cn", "api.kimi.com", "platform.kimi.ai"}, aliases: []string{"moonshot", "kimi-coding"}},
	{provider: "minimax", hosts: []string{"api.minimax.io", "api.minimaxi.com"}, aliases: []string{"minimax", "minimax-china"}},
	{provider: "deepseek", hosts: []string{"api.deepseek.com"}, aliases: []string{"deepseek"}},
	{provider: "dashscope", hosts: []string{"dashscope.aliyuncs.com", "dashscope-intl.aliyuncs.com", "coding.dashscope.aliyuncs.com", "coding-intl.dashscope.aliyuncs.com"}, aliases: []string{"dashscope", "alibaba-singapore"}},
	{provider: "openai", hosts: []string{"api.openai.com"}, aliases: []string{"openai"}},
	{provider: "azure", aliases: []string{"azure"}, hostPattern: "azure"},
	{provider: "openrouter", hosts: []string{"openrouter.ai"}, aliases: []string{"openrouter"}},
	{provider: "xai", hosts: []string{"api.x.ai"}, aliases: []string{"xai"}},
	{provider: "groq", hosts: []string{"api.groq.com"}, aliases: []string{"groq"}},
	{provider: "gemini", hosts: []string{"generativelanguage.googleapis.com", "aiplatform.googleapis.com"}, aliases: []string{"gemini", "vertexai"}, hostPattern: "vertex"},
	{provider: "mistral", hosts: []string{"api.mistral.ai"}, aliases: []string{"mistral"}},
	{provider: "bedrock", aliases: []string{"bedrock"}, hostPattern: "bedrock"},
	{provider: "stepfun", hosts: []string{"api.stepfun.ai"}, aliases: []string{"stepfun"}},
	{provider: "zai", hosts: []string{"api.z.ai", "open.z.ai", "open.bigmodel.cn"}, aliases: []string{"zai"}},
	{provider: "openai-codex", hosts: []string{"chatgpt.com"}, aliases: []string{"openai-codex"}},
	{provider: "deepseek", statuses: []int{402}, class: Hard},
	{provider: "openrouter", statuses: []int{402}, class: Hard},
	{provider: "minimax", statuses: []int{429, 500}, codes: []string{"1008", "2056"}, words: []string{"insufficient balance (1008)", "usage limit exceeded (2056)"}, class: Hard},
	{provider: "minimax", statuses: []int{429}, codes: []string{"1002"}, class: Transient},
	{provider: "moonshot", statuses: []int{429}, codes: []string{"exceeded_current_quota_error"}, class: Hard},
	{provider: "moonshot", statuses: []int{429}, codes: []string{"rate_limit_reached_error", "engine_overloaded_error"}, class: Transient},
	{provider: "dashscope", statuses: []int{429}, codes: []string{"prepaidbilloverdue", "postpaidbilloverdue", "arrearage"}, words: []string{"free allocated quota exceeded", "hour allocated quota", "weekly allocated quota", "week allocated quota", "monthly allocated quota", "month allocated quota"}, class: Hard},
	{provider: "dashscope", statuses: []int{429}, codes: []string{"throttling.allocationquota", "insufficient_quota", "throttling.ratequota", "limitrequests"}, words: []string{"allocated quota exceeded, please increase your quota limit", "throttling."}, class: Transient},
	{provider: "azure", statuses: []int{429}, class: Transient},
	{provider: "groq", statuses: []int{429}, words: []string{"tokens per day", "tpd"}, class: Hard},
	{provider: "bedrock", statuses: []int{429}, words: []string{"too many tokens per day"}, class: Hard},
	{provider: "xai", statuses: []int{429}, words: []string{"used all available credits", "used all credits", "monthly spending limit"}, class: Hard},
	{provider: "gemini", statuses: []int{429}, codes: []string{"quota_per_day"}, class: Hard},
	{provider: "gemini", statuses: []int{429}, codes: []string{"quota_per_minute"}, class: Transient},
	{provider: "openrouter", statuses: []int{429}, class: Transient},
	{provider: "deepseek", statuses: []int{429, 503}, class: Transient},
	{provider: "mistral", statuses: []int{429}, codes: []string{"rate_limited", "1300"}, words: []string{"rate limit exceeded"}, class: Transient},
	{provider: "bedrock", statuses: []int{429}, codes: []string{"throttlingexception"}, words: []string{"too many requests"}, class: Transient},
	{provider: "xai", statuses: []int{429}, words: []string{"rps", "tpm", "per model", "per-model"}, class: Transient},
	{provider: "groq", statuses: []int{429}, words: []string{"tpm", "rpm"}, class: Transient},
	// Generic OpenAI-compatible fallback, after provider exceptions.
	{provider: "*", statuses: []int{429}, codes: []string{"insufficient_quota"}, words: []string{"exceeded your current quota", "current quota, please check your plan and billing"}, class: Hard},
}

// ProviderIdentity resolves serving hosts before aliases, not model names.
func ProviderIdentity(rawURL, hint string) string {
	parsed, _ := url.Parse(rawURL)
	host := ""
	if parsed != nil {
		host = strings.ToLower(parsed.Hostname())
	}
	for _, rule := range providerRules {
		for _, exact := range rule.hosts {
			if host == exact {
				return rule.provider
			}
		}
		if matchesProviderHost(host, rule.hostPattern) {
			return rule.provider
		}
	}
	for _, rule := range providerRules {
		for _, alias := range rule.aliases {
			if strings.EqualFold(hint, alias) {
				return rule.provider
			}
		}
	}
	return ""
}

func matchesProviderHost(host, pattern string) bool {
	switch pattern {
	case "azure":
		return strings.HasSuffix(host, ".openai.azure.com") || strings.HasSuffix(host, ".cognitiveservices.azure.com")
	case "vertex":
		region, ok := strings.CutSuffix(host, "-aiplatform.googleapis.com")
		return ok && region != "" && !strings.Contains(region, ".")
	case "bedrock":
		region, ok := strings.CutPrefix(host, "bedrock-runtime.")
		region, suffix := strings.CutSuffix(region, ".amazonaws.com")
		return ok && suffix && region != "" && !strings.Contains(region, ".")
	default:
		return false
	}
}

func providerRuleClass(input Input, obj map[string]any) Class {
	codes := []string{normalizeLimitText(Field(obj, "code")), normalizeLimitText(Field(obj, "type")), normalizeLimitText(Field(obj, "status_code"))}
	text := normalizeLimitText(input.Message + " " + Field(obj, "message") + " " + Field(obj, "status_msg"))
	day, minute, _ := GeminiDetails(obj)
	if day {
		codes = append(codes, "quota_per_day")
	} else if minute {
		codes = append(codes, "quota_per_minute")
	}
	for _, rule := range providerRules {
		if rule.provider != "*" && rule.provider != normalizeLimitText(input.Provider) {
			continue
		}
		statusOK := false
		for _, status := range rule.statuses {
			if input.Status == status {
				statusOK = true
			}
		}
		if !statusOK {
			continue
		}
		match := len(rule.codes) == 0 && len(rule.words) == 0
		for _, expected := range rule.codes {
			for _, code := range codes {
				if code == expected {
					match = true
				}
			}
		}
		for _, word := range rule.words {
			if strings.Contains(text, word) {
				match = true
			}
		}
		if match {
			return rule.class
		}
	}
	return Unknown
}

var groqRetryPattern = regexp.MustCompile(`(?i)try again in\s+([0-9]+(?:\.[0-9]+)?[hms](?:[0-9]+(?:\.[0-9]+)?[hms])*)`)

// GroqRetryDelay reads only the failure-message duration, not arbitrary numbers.
func GroqRetryDelay(message string) time.Duration {
	match := groqRetryPattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return 0
	}
	duration, _ := time.ParseDuration(strings.ToLower(match[1]))
	return duration
}

// GeminiDetails extracts typed Google RPC details, preserving daily precedence.
func GeminiDetails(obj map[string]any) (day, minute bool, delay time.Duration) {
	details, _ := obj["details"].([]any)
	for _, value := range details {
		detail, _ := value.(map[string]any)
		switch Field(detail, "@type") {
		case "type.googleapis.com/google.rpc.QuotaFailure":
			violations, _ := detail["violations"].([]any)
			for _, value := range violations {
				violation, _ := value.(map[string]any)
				id := strings.ToLower(Field(violation, "quotaId"))
				day = day || strings.Contains(id, "perday")
				minute = minute || strings.Contains(id, "perminute")
			}
		case "type.googleapis.com/google.rpc.RetryInfo":
			duration, err := time.ParseDuration(Field(detail, "retryDelay"))
			if err == nil && duration > delay {
				delay = duration
			}
		}
	}
	return day, minute, delay
}
