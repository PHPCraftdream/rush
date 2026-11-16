// CL-A: local copies of unexported helpers that moved to clipspec but are
// still referenced by facade-package tests that stay until CL-C (design
// "medium" §4.4: a helper name crossing a package boundary is closed by a
// local copy, not by exporting). The originals live in clipspec; these
// copies are byte-equivalent ports and are deleted when the stream-cluster
// tests move to clipstream in CL-C.
package cliprovider

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"charm.land/fantasy"
)

// claudeEffortLevels is what `claude --help` documents for --effort.
// (local copy; original: clipspec/effort.go)
var claudeEffortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// applyClaudeEffort sets --effort, replacing a value already placed there by
// BuildArgs so a UI toggle wins over the static default.
// (local copy; original: clipspec/effort.go)
func applyClaudeEffort(args []string, effort string) []string {
	for i, a := range args {
		if a == "--effort" && i+1 < len(args) {
			args[i+1] = effort
			return args
		}
	}
	return append(args, "--effort", effort)
}

// (local copy; original: clipspec/provider_specs.go)
func claudeArgs(model string, extra ...string) func(bool) []string {
	return func(yolo bool) []string {
		args := []string{
			"--model", model,
			"--output-format", "stream-json",
			"--verbose",
			"--include-partial-messages",
		}
		args = append(args, extra...)
		if yolo {
			args = append(args, "--dangerously-skip-permissions")
		}
		return args
	}
}

// (local copy; original: clipspec/provider_specs.go)
func claudeSpec(modelID, modelName, modelArg string, ctxWindow int64) CLISpec {
	return CLISpec{
		ModelID:                 modelID,
		ModelName:               modelName,
		ContextWindow:           ctxWindow,
		Binary:                  "claude",
		PromptFlag:              "-p",
		StdinPrintFlag:          "-p",
		BuildArgs:               claudeArgs(modelArg),
		NewPartParser:           claudePartParser,
		ParseUsageLine:          claudeParseUsageLine,
		UseRushMCP:              true,
		SupportsRestrictedTools: true,
		SupportsResume:          true,
		ApplyEffort:             applyClaudeEffort,
		EffortLevels:            claudeEffortLevels,
	}
}

// (local copy; original: clipspec/usage.go)
type rawUsage struct {
	input  int64
	output int64

	cacheCreation int64
	cacheRead     int64

	reasoning int64

	inputIncludesCache bool

	reportsCache bool

	totalOverride int64

	model string
}

// (local copy; original: clipspec/usage.go)
func (r rawUsage) promptTotal() int64 {
	if r.inputIncludesCache {
		return r.input
	}
	return r.input + r.cacheCreation + r.cacheRead
}

// (local copy; original: clipspec/usage.go)
func (r rawUsage) normalize() fantasy.Usage {
	inputExclusive := r.input
	if r.inputIncludesCache {
		inputExclusive = r.input - r.cacheRead - r.cacheCreation
		if inputExclusive < 0 {
			slog.Warn(
				"cliprovider: provider reported more cached tokens than total input; clamping",
				"input", r.input,
				"cacheRead", r.cacheRead,
				"cacheCreation", r.cacheCreation,
				"model", r.model,
			)
			inputExclusive = 0
		}
	}

	total := r.totalOverride
	if total == 0 {
		total = r.promptTotal() + r.output
	}

	return fantasy.Usage{
		InputTokens:         inputExclusive,
		OutputTokens:        r.output,
		ReasoningTokens:     r.reasoning,
		CacheCreationTokens: r.cacheCreation,
		CacheReadTokens:     r.cacheRead,
		TotalTokens:         total,
	}
}

// (local copy; original: clipspec/usage.go)
func (r rawUsage) isEmpty() bool {
	return r.promptTotal() == 0 && r.output == 0
}

// (local copy; original: clipspec/usage.go)
func logUsage(binary string, r rawUsage, u fantasy.Usage) {
	attrs := []any{
		"binary", binary,
		"input", u.InputTokens,
		"cache_create", u.CacheCreationTokens,
		"cache_read", u.CacheReadTokens,
		"output", u.OutputTokens,
		"prompt_total", r.promptTotal(),
	}
	if r.model != "" {
		attrs = append(attrs, "resolved_model", r.model)
	}
	if !r.reportsCache {
		attrs = append(attrs, "cache_hit_pct", "n/a")
	} else if total := r.promptTotal(); total > 0 {
		attrs = append(attrs, "cache_hit_pct",
			fmt.Sprintf("%.1f%%", float64(u.CacheReadTokens)/float64(total)*100))
	}
	slog.Info("cliprovider: token usage", attrs...)
}

// (local copy; original: clipspec/provider_parsing.go)
func claudeParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev streamEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "result" {
		return fantasy.Usage{}, false
	}
	raw := rawUsage{
		input:              ev.Usage.InputTokens,
		output:             ev.Usage.OutputTokens,
		cacheCreation:      ev.Usage.CacheCreationInputTokens,
		cacheRead:          ev.Usage.CacheReadInputTokens,
		inputIncludesCache: false,
		reportsCache:       true,
	}
	if raw.isEmpty() {
		return fantasy.Usage{}, false
	}
	u := raw.normalize()
	logUsage("claude", raw, u)
	return u, true
}

// (local copy; original: clipspec/provider_parsing.go)
func geminiParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev geminiCLIEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "result" {
		return fantasy.Usage{}, false
	}
	raw := rawUsage{
		input:              ev.Stats.InputTokens,
		output:             ev.Stats.OutputTokens,
		cacheRead:          ev.Stats.Cached,
		inputIncludesCache: true,
		reportsCache:       true,
		totalOverride:      ev.Stats.TotalTokens,
	}
	if raw.isEmpty() {
		return fantasy.Usage{}, false
	}
	u := raw.normalize()
	logUsage("gemini", raw, u)
	return u, true
}

// (local copy; original: clipspec/provider_parsing.go)
func codexParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "turn.completed" {
		return fantasy.Usage{}, false
	}
	raw := rawUsage{
		input:              ev.Usage.InputTokens,
		output:             ev.Usage.OutputTokens,
		cacheRead:          ev.Usage.CachedInputTokens,
		cacheCreation:      ev.Usage.CacheWriteInputTokens,
		reasoning:          ev.Usage.ReasoningOutputTokens,
		inputIncludesCache: true,
		reportsCache:       true,
	}
	if raw.isEmpty() {
		return fantasy.Usage{}, false
	}
	u := raw.normalize()
	logUsage("codex", raw, u)
	return u, true
}

// (local copy; original: clipspec/provider_parsing.go)
type geminiCLIEvent struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Delta   bool   `json:"delta"`
	Status  string `json:"status"`
	Stats   struct {
		TotalTokens  int64 `json:"total_tokens"`
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		Cached       int64 `json:"cached"`
		Input        int64 `json:"input"`
	} `json:"stats"`
}

// (local copy; original: clipspec/provider_parsing.go)
type codexEvent struct {
	Type string `json:"type"`
	Item struct {
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
	} `json:"item"`
	Usage struct {
		InputTokens           int64 `json:"input_tokens"`
		CachedInputTokens     int64 `json:"cached_input_tokens"`
		CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
		OutputTokens          int64 `json:"output_tokens"`
		ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
}

// (local copy; original: clipspec/provider_parsing.go)
func claudePartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	var inThinking bool
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type != "stream_event" {
			return fantasy.StreamPart{}, false
		}
		switch ev.Event.Type {
		case "content_block_start":
			slog.Debug("cliprovider: content_block_start", "block_type", ev.Event.ContentBlock.Type)
			if ev.Event.ContentBlock.Type == "thinking" {
				inThinking = true
				return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: id}, true
			}
		case "content_block_stop":
			if inThinking {
				inThinking = false
				return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id}, true
			}
		case "content_block_delta":
			switch ev.Event.Delta.Type {
			case "thinking_delta":
				if ev.Event.Delta.Thinking != "" {
					return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: id, Delta: ev.Event.Delta.Thinking}, true
				}
			case "text_delta":
				if ev.Event.Delta.Text != "" {
					return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Event.Delta.Text}, true
				}
			}
		}
		return fantasy.StreamPart{}, false
	}
}

// (local copy; original: clipspec/provider_parsing.go)
func geminiPartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev geminiCLIEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type == "message" && ev.Role == "assistant" && ev.Content != "" {
			return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Content}, true
		}
		return fantasy.StreamPart{}, false
	}
}

// (local copy; original: clipspec/provider_parsing.go)
func codexPartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev codexEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type == "item.completed" && ev.Item.Type == "agent_message" && ev.Item.Text != "" {
			return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Item.Text}, true
		}
		return fantasy.StreamPart{}, false
	}
}
