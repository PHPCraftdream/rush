package turnstep

import (
	"regexp"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/contract/agentcall"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"
)

// Used to remove <think> tags from generated titles.
var (
	thinkTagRegex       = regexp.MustCompile(`(?s)<think>.*?</think>`)
	orphanThinkTagRegex = regexp.MustCompile(`</?think>`)
)

// CleanTitle normalises a raw model title response: collapse newlines, strip
// any (orphan) think tags, and trim. Returns "" when nothing usable remains
// (e.g. a pure-reasoning response with no actual title text).
func CleanTitle(raw string) string {
	t := thinkTagRegex.ReplaceAllString(raw, "")
	t = orphanThinkTagRegex.ReplaceAllString(t, "")
	t = strings.ReplaceAll(t, "\r\n", " ")
	t = strings.ReplaceAll(t, "\r", " ")
	t = strings.ReplaceAll(t, "\n", " ")
	t = strings.TrimSpace(t)
	if len([]rune(t)) > titleMaxReasoningLen || hasMultipleTitleSentences(t) {
		return ""
	}
	return t
}

func hasMultipleTitleSentences(title string) bool {
	sentences := 0
	for _, r := range title {
		if r == '.' || r == '!' || r == '?' || r == '。' || r == '！' || r == '？' {
			sentences++
			if sentences > 1 {
				return true
			}
		}
	}
	return false
}

// TitleFromReasoning extracts a title candidate from a reasoning-only
// response. Thinking models (z.ai/glm via the openai-compat provider) can
// stream their whole answer as `reasoning_content`, leaving
// ResponseContent.Text() empty; the final answer typically sits on the last
// non-empty line of the reasoning stream, so that line is the best title
// candidate.
func TitleFromReasoning(reasoning string) string {
	lines := strings.Split(reasoning, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

// titleMaxReasoningLen caps a title recovered from a reasoning line.
const titleMaxReasoningLen = 80

// SanitizeReasoningTitle cleans a title candidate recovered from a reasoning
// stream: strip answer prefixes ("Title:"/"Заголовок:" and friends) and
// wrapping quotes, then cap the length — reasoning lines can run long and a
// title must stay short. A cut longer than titleMaxReasoningLen falls back
// to the last space inside the window so no word is split mid-token.
func SanitizeReasoningTitle(raw string) string {
	t := CleanTitle(raw)
	if t == "" {
		return ""
	}

	for _, p := range []string{"Title:", "Заголовок:", "Title -", "Заголовок -"} {
		if len(t) >= len(p) && strings.EqualFold(t[:len(p)], p) {
			t = strings.TrimSpace(t[len(p):])
		}
	}
	t = strings.Trim(t, "\"'«»„“”")
	runes := []rune(t)
	if len(runes) <= titleMaxReasoningLen {
		return t
	}
	cut := titleMaxReasoningLen
	if sp := strings.LastIndex(string(runes[:titleMaxReasoningLen]), " "); sp > titleMaxReasoningLen/2 {
		cut = sp
	}
	return strings.TrimSpace(string(runes[:cut]))
}

// TitleProviderOptions returns the request-level options that stop a
// thinking-by-default openai-compat provider (classified by the CONFIGURED
// provider ID, the same domain getProviderOptions switches on) from thinking
// on the title call. The prompt-level "/no_think" and "<think></think>"
// tricks are ignored by z.ai's GLM models, so without this the whole title
// answer streams as reasoning_content and Response.Content.Text() is empty.
// GLM-5.3 model IDs cannot disable thinking at all (see zai53ModelIDs), so
// they get their minimal level instead — mirroring getProviderOptions'
// "off" → "low" degradation. Any other provider keeps its default behavior.
func TitleProviderOptions(model agentcall.Model) *fantasy.ProviderOptions {
	if model.ModelCfg.Provider != string(catwalk.InferenceProviderZAI) {
		return nil
	}
	extraBody := map[string]any{}
	if modelkit.IsZAI53Model(strings.ToLower(model.ModelCfg.Model)) {
		extraBody["thinking"] = map[string]any{"type": "enabled"}
		extraBody["reasoning_effort"] = "low"
	} else {
		extraBody["thinking"] = map[string]any{"type": "disabled"}
	}
	parsed, err := openaicompat.ParseOptions(map[string]any{"extra_body": extraBody})
	if err != nil {
		return nil
	}
	return &fantasy.ProviderOptions{openaicompat.Name: parsed}
}
