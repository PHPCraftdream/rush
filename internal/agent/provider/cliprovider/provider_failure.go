package cliprovider

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// cliFailureEvidence is separate from arbitrary JSON and noisy stderr diagnostics.
type cliFailureEvidence struct {
	seen            bool
	text            string
	hard, transient bool
}

func cliFailureName(spec CLISpec) string {
	for _, name := range []string{"codex", "claude", "qwen"} {
		if strings.HasPrefix(spec.ModelID, "cli-"+name) || strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(filepath.Base(spec.Binary)), ".exe"), ".cmd") == name {
			return name
		}
	}
	return ""
}

// cliFailureText accepts only explicit failure channels, never content events.
func cliFailureText(name string, line []byte) (string, bool) {
	var ev struct {
		Type    string          `json:"type"`
		IsError bool            `json:"is_error"`
		Result  string          `json:"result"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line, &ev) != nil {
		return "", false
	}
	errorText := func() string {
		var text string
		if json.Unmarshal(ev.Error, &text) == nil {
			return text
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(ev.Error, &obj) == nil {
			return obj.Message
		}
		return ""
	}
	switch name {
	case "codex":
		switch ev.Type {
		case "error":
			return ev.Message, true
		case "turn.failed":
			return errorText(), true
		}
	case "claude", "qwen":
		if ev.Type == "result" && ev.IsError {
			return strings.TrimSpace(strings.Join([]string{ev.Result, errorText(), ev.Message}, "\n")), true
		}
	}
	return "", false
}

func (e *cliFailureEvidence) add(name string, line []byte) {
	text, ok := cliFailureText(name, line)
	if !ok {
		return
	}
	e.seen = true
	hard, transient := cliLimitText(text)
	e.transient = e.transient || transient
	// Classify before bounding, and reserve failure evidence independently of stderr.
	if e.text == "" || hard || transient {
		e.text = boundedFailureText(strings.TrimSpace(e.text + "\n" + text))
	}
	e.hard = e.hard || hard
}

// boundedFailureText reserves keyword/reset contexts before clipping noisy text.
func boundedFailureText(text string) string {
	if len(text) <= maxExitDiagnostics {
		return text
	}
	lower := strings.ToLower(text)
	out := text[:1024]
	for _, word := range []string{"usage limit", "hit your limit", "hit your usage limit", "limit reached", "limit will reset", "reset at", "resets ", "try again at", "quota", "usage_limit_reached", "per minute", "overloaded", "requests"} {
		if i := strings.Index(lower, word); i >= 0 {
			start, end := max(0, i-64), min(len(text), i+320)
			out += "\n...\n" + text[start:end]
		}
	}
	out += "\n...\n" + text[len(text)-1024:]
	return out
}
